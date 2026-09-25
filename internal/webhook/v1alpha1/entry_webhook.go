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
	"context"

	klapv1alpha1 "github.com/genesary/klap/api/v1alpha1"
	"github.com/genesary/klap/internal/controller"
	"github.com/go-ldap/ldap/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// nolint:unused
// log is for logging in this package.
var entrylog = logf.Log.WithName("entry-resource")

// SetupEntryWebhookWithManager registers the webhook for Entry in the manager.
func SetupEntryWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &klapv1alpha1.Entry{}).
		WithValidator(&EntryValidator{}).
		WithDefaulter(&EntryDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-klap-genesary-github-com-v1alpha1-entry,mutating=true,failurePolicy=fail,sideEffects=None,groups=klap.genesary.github.com,resources=entries,verbs=create;update,versions=v1alpha1,name=mentry-v1alpha1.kb.io,admissionReviewVersions=v1

// EntryDefaulter struct is responsible for setting default values on the custom resource of the
// Kind Entry when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type EntryDefaulter struct {
	// TODO(user): Add more fields as needed for defaulting
}

// Default implements admission.Defaulter so a webhook will be registered for the Kind Entry.
func (d *EntryDefaulter) Default(_ context.Context, obj *klapv1alpha1.Entry) error {
	entrylog.Info("Defaulting for Entry", "name", obj.GetName())

	if obj.DeletionTimestamp == nil && !controllerutil.ContainsFinalizer(obj, controller.Finalizer) {
		controllerutil.AddFinalizer(obj, controller.Finalizer)
	}

	if obj.Spec.ServerRef.Namespace == nil {
		obj.Spec.ServerRef.Namespace = &obj.Namespace
	}

	return nil
}

// +kubebuilder:webhook:path=/validate-klap-genesary-github-com-v1alpha1-entry,mutating=false,failurePolicy=fail,sideEffects=None,groups=klap.genesary.github.com,resources=entries,verbs=create;update,versions=v1alpha1,name=ventry-v1alpha1.kb.io,admissionReviewVersions=v1

// EntryValidator struct is responsible for validating the Entry resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type EntryValidator struct {
	// TODO(user): Add more fields as needed for validation
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type Entry.
func (v *EntryValidator) ValidateCreate(_ context.Context, obj *klapv1alpha1.Entry) (admission.Warnings, error) {
	entrylog.Info("Validation for Entry upon creation", "name", obj.GetName())

	return nil, validateEntry(obj)
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type Entry.
func (v *EntryValidator) ValidateUpdate(_ context.Context, oldObj, newObj *klapv1alpha1.Entry) (admission.Warnings, error) {
	entrylog.Info("Validation for Entry upon update", "name", newObj.GetName())

	return nil, validateEntry(newObj)
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type Entry.
func (v *EntryValidator) ValidateDelete(_ context.Context, obj *klapv1alpha1.Entry) (admission.Warnings, error) {
	entrylog.Info("Validation for Entry upon deletion", "name", obj.GetName())

	return nil, nil
}

// validateEntry performs validation on the Entry resource.
func validateEntry(entry *klapv1alpha1.Entry) error {
	var allErrs field.ErrorList

	if _, err := ldap.ParseDN(*entry.Spec.DN); err != nil {
		fieldErr := field.Invalid(field.NewPath("spec").Child("dn"), entry.Spec.DN, "must be a valid distinguished name")
		allErrs = append(allErrs, fieldErr)
	}

	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		schema.GroupKind{Group: "klap.genesary.github.com", Kind: "Entry"},
		entry.Name, allErrs)
}
