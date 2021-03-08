/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package conversion

import (
	"context"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	fuzz "github.com/google/gofuzz"
	"github.com/onsi/gomega"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/apitesting/fuzzer"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metafuzzer "k8s.io/apimachinery/pkg/apis/meta/fuzzer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha4"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

const (
	DataAnnotation = "cluster.x-k8s.io/conversion-data"
)

var (
	contract = clusterv1.GroupVersion.String()
)

// ConvertReferenceAPIContract takes a client and object reference, queries the API Server for
// the Custom Resource Definition and looks which one is the stored version available.
//
// The object passed as input is modified in place if an updated compatible version is found.
func ConvertReferenceAPIContract(ctx context.Context, c client.Client, restConfig *rest.Config, ref *corev1.ObjectReference) error {
	log := ctrl.LoggerFrom(ctx)
	gvk := ref.GroupVersionKind()

	metadata, err := util.GetCRDMetadataFromGVK(ctx, restConfig, gvk)
	if err != nil {
		log.Info("Cannot retrieve CRD with metadata only client, falling back to slower listing", "err", err.Error())
		// Fallback to slower and more memory intensive method to get the full CRD.
		crd, err := util.GetCRDWithContract(ctx, c, gvk, contract)
		if err != nil {
			return err
		}
		metadata = &metav1.PartialObjectMetadata{
			TypeMeta:   crd.TypeMeta,
			ObjectMeta: crd.ObjectMeta,
		}
	}

	// If there is no label, return early without changing the reference.
	supportedVersions, ok := metadata.Labels[contract]
	if !ok || supportedVersions == "" {
		return errors.Errorf("cannot find any versions matching contract %q for CRD %v as contract version label(s) are either missing or empty", contract, metadata.Name)
	}

	// Pick the latest version in the slice and validate it.
	kubeVersions := util.KubeAwareAPIVersions(strings.Split(supportedVersions, "_"))
	sort.Sort(kubeVersions)
	chosen := kubeVersions[len(kubeVersions)-1]

	// Modify the GroupVersionKind with the new version.
	if gvk.Version != chosen {
		gvk.Version = chosen
		ref.SetGroupVersionKind(gvk)
	}

	return nil
}

// MarshalData stores the source object as json data in the destination object annotations map.
// It ignores the metadata of the source object.
func MarshalData(src metav1.Object, dst metav1.Object) error {
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(src)
	if err != nil {
		return err
	}
	delete(u, "metadata")

	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	if dst.GetAnnotations() == nil {
		dst.SetAnnotations(map[string]string{})
	}
	dst.GetAnnotations()[DataAnnotation] = string(data)
	return nil
}

// UnmarshalData tries to retrieve the data from the annotation and unmarshals it into the object passed as input.
func UnmarshalData(from metav1.Object, to interface{}) (bool, error) {
	data, ok := from.GetAnnotations()[DataAnnotation]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal([]byte(data), to); err != nil {
		return false, err
	}
	delete(from.GetAnnotations(), DataAnnotation)
	return true, nil
}

// GetFuzzer returns a new fuzzer to be used for testing.
func GetFuzzer(scheme *runtime.Scheme, funcs ...fuzzer.FuzzerFuncs) *fuzz.Fuzzer {
	funcs = append([]fuzzer.FuzzerFuncs{metafuzzer.Funcs}, funcs...)
	return fuzzer.FuzzerFor(
		fuzzer.MergeFuzzerFuncs(funcs...),
		rand.NewSource(rand.Int63()),
		serializer.NewCodecFactory(scheme),
	)
}

// FuzzTestFunc returns a new testing function to be used in tests to make sure conversions between
// the Hub version of an object and an older version aren't lossy.
func FuzzTestFunc(scheme *runtime.Scheme, hub conversion.Hub, dst conversion.Convertible, funcs ...fuzzer.FuzzerFuncs) func(*testing.T) {
	return func(t *testing.T) {
		t.Run("spoke-hub-spoke", func(t *testing.T) {
			g := gomega.NewWithT(t)
			fuzzer := GetFuzzer(scheme, funcs...)

			for i := 0; i < 10000; i++ {
				// Create the spoke and fuzz it
				spokeBefore := dst.DeepCopyObject().(conversion.Convertible)
				fuzzer.Fuzz(spokeBefore)

				// First convert spoke to hub
				hubCopy := hub.DeepCopyObject().(conversion.Hub)
				g.Expect(spokeBefore.ConvertTo(hubCopy)).To(gomega.Succeed())

				// Convert hub back to spoke and check if the resulting spoke is equal to the spoke before the round trip
				spokeAfter := dst.DeepCopyObject().(conversion.Convertible)
				g.Expect(spokeAfter.ConvertFrom(hubCopy)).To(gomega.Succeed())

				// Remove data annotation eventually added by ConvertFrom for avoiding data loss in hub-spoke-hub round trips
				metaAfter := spokeAfter.(metav1.Object)
				delete(metaAfter.GetAnnotations(), DataAnnotation)

				g.Expect(apiequality.Semantic.DeepEqual(spokeBefore, spokeAfter)).To(gomega.BeTrue(), cmp.Diff(spokeBefore, spokeAfter))
			}
		})
		t.Run("hub-spoke-hub", func(t *testing.T) {
			g := gomega.NewWithT(t)
			fuzzer := GetFuzzer(scheme, funcs...)

			for i := 0; i < 10000; i++ {
				// Create the hub and fuzz it
				hubBefore := hub.DeepCopyObject().(conversion.Hub)
				fuzzer.Fuzz(hubBefore)

				// First convert hub to spoke
				dstCopy := dst.DeepCopyObject().(conversion.Convertible)
				g.Expect(dstCopy.ConvertFrom(hubBefore)).To(gomega.Succeed())

				// Convert spoke back to hub and check if the resulting hub is equal to the hub before the round trip
				hubAfter := hub.DeepCopyObject().(conversion.Hub)
				g.Expect(dstCopy.ConvertTo(hubAfter)).To(gomega.Succeed())

				g.Expect(apiequality.Semantic.DeepEqual(hubBefore, hubAfter)).To(gomega.BeTrue(), cmp.Diff(hubBefore, hubAfter))
			}
		})
	}
}
