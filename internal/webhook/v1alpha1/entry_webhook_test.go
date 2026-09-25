/*
Copyright 2025.

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

package v1alpha1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	klapv1alpha1 "github.com/genesary/klap/api/v1alpha1"
	// TODO (user): Add any additional imports if needed

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Entry Webhook", func() {
	var (
		obj       *klapv1alpha1.Entry
		oldObj    *klapv1alpha1.Entry
		validator EntryValidator
		defaulter EntryDefaulter
	)

	BeforeEach(func() {
		obj = &klapv1alpha1.Entry{}
		oldObj = &klapv1alpha1.Entry{}
		validator = EntryValidator{}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		defaulter = EntryDefaulter{}
		Expect(defaulter).NotTo(BeNil(), "Expected defaulter to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
	})

	AfterEach(func() {
		// TODO (user): Add any teardown logic common to all tests
	})

	Context("When creating Entry under Defaulting Webhook", func() {
		// TODO (user): Add logic for defaulting webhooks
		// Example:
		// It("Should apply defaults when a required field is empty", func() {
		//     By("simulating a scenario where defaults should be applied")
		//     obj.SomeFieldWithDefault = ""
		//     By("calling the Default method to apply defaults")
		//     defaulter.Default(ctx, obj)
		//     By("checking that the default values are set")
		//     Expect(obj.SomeFieldWithDefault).To(Equal("default_value"))
		// })
		It("Should apply defaults when a required field is empty", func() {
			By("simulating a scenario where defaults should be applied")
			obj.ObjectMeta = metav1.ObjectMeta{
				Name:      "test-entry",
				Namespace: "default",
			}
			serverName := "my-server"
			obj.Spec.ServerRef.Name = &serverName
			By("calling the Default method to apply defaults")
			err := defaulter.Default(ctx, obj)
			By("checking that the default values are set")
			Expect(err).ToNot(HaveOccurred())
			Expect(*obj.Spec.ServerRef.Namespace).To(Equal(obj.Namespace))
			Expect(obj.Finalizers).To(HaveLen(1))
		})
	})

	Context("When creating or updating Entry under Validating Webhook", func() {
		// TODO (user): Add logic for validating webhooks
		// Example:
		// It("Should deny creation if a required field is missing", func() {
		//     By("simulating an invalid creation scenario")
		//     obj.SomeRequiredField = ""
		//     Expect(validator.ValidateCreate(ctx, obj)).Error().To(HaveOccurred())
		// })
		//
		// It("Should admit creation if all required fields are present", func() {
		//     By("simulating an invalid creation scenario")
		//     obj.SomeRequiredField = "valid_value"
		//     Expect(validator.ValidateCreate(ctx, obj)).To(BeNil())
		// })
		//
		// It("Should validate updates correctly", func() {
		//     By("simulating a valid update scenario")
		//     oldObj.SomeRequiredField = "updated_value"
		//     obj.SomeRequiredField = "updated_value"
		//     Expect(validator.ValidateUpdate(ctx, oldObj, obj)).To(BeNil())
		// })
		It("Should deny creation if a required field is not well formated", func() {
			By("simulating an invalid creation scenario")
			dn := "foobar"
			obj.Spec = klapv1alpha1.EntrySpec{
				DN: &dn,
			}
			Expect(validator.ValidateCreate(ctx, obj)).Error().To(HaveOccurred())
		})
		It("Should validate creates/updates correctly", func() {
			By("simulating a valid update scenario")
			dn := "cn=foobar"
			obj.Spec = klapv1alpha1.EntrySpec{
				DN: &dn,
			}
			Expect(validator.ValidateUpdate(ctx, oldObj, obj)).To(BeNil())
		})
	})

})
