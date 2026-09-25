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
	"fmt"
	"net/url"
	"regexp"
	"slices"

	klapv1alpha1 "github.com/genesary/klap/api/v1alpha1"
	"github.com/genesary/klap/internal/util/boolptr"
	"github.com/go-ldap/ldap/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	CACertName = "ca.crt"
	Password   = "password"
)

// nolint:unused
// log is for logging in this package.
var serverlog = logf.Log.WithName("server-resource")

// SetupServerWebhookWithManager registers the webhook for Server in the manager.
func SetupServerWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &klapv1alpha1.Server{}).
		WithValidator(&ServerValidator{}).
		WithDefaulter(&ServerDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-klap-genesary-github-com-v1alpha1-server,mutating=true,failurePolicy=fail,sideEffects=None,groups=klap.genesary.github.com,resources=servers,verbs=create;update,versions=v1alpha1,name=mserver-v1alpha1.kb.io,admissionReviewVersions=v1

// ServerDefaulter struct is responsible for setting default values on the custom resource of the
// Kind Server when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type ServerDefaulter struct {
	// TODO(user): Add more fields as needed for defaulting
}

// Default implements admission.Defaulter so a webhook will be registered for the Kind Server.
func (d *ServerDefaulter) Default(_ context.Context, obj *klapv1alpha1.Server) error {
	serverlog.Info("Defaulting for Server", "name", obj.GetName())

	if obj.Spec.PasswordSecretRef.Key == nil {
		key := Password
		obj.Spec.PasswordSecretRef.Key = &key
	}

	if obj.Spec.TlsSecretRef.Name != nil {
		if obj.Spec.TlsSecretRef.Key == nil {
			key := CACertName
			obj.Spec.TlsSecretRef.Key = &key
		}
	}

	return nil
}

// NOTE: If you want to customise the 'path', use the flags '--defaulting-path' or '--validation-path'.
// +kubebuilder:webhook:path=/validate-klap-genesary-github-com-v1alpha1-server,mutating=false,failurePolicy=fail,sideEffects=None,groups=klap.genesary.github.com,resources=servers,verbs=create;update,versions=v1alpha1,name=vserver-v1alpha1.kb.io,admissionReviewVersions=v1

// ServerValidator struct is responsible for validating the Server resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type ServerValidator struct {
	// TODO(user): Add more fields as needed for validation
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type Server.
func (v *ServerValidator) ValidateCreate(_ context.Context, obj *klapv1alpha1.Server) (admission.Warnings, error) {
	serverlog.Info("Validation for Server upon creation", "name", obj.GetName())

	return validateServer(obj)
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type Server.
func (v *ServerValidator) ValidateUpdate(_ context.Context, oldObj, newObj *klapv1alpha1.Server) (admission.Warnings, error) {
	serverlog.Info("Validation for Server upon update", "name", newObj.GetName())

	return validateServer(newObj)
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type Server.
func (v *ServerValidator) ValidateDelete(_ context.Context, obj *klapv1alpha1.Server) (admission.Warnings, error) {
	serverlog.Info("Validation for Server upon deletion", "name", obj.GetName())

	return nil, nil
}

// validateServer performs the actual validation of the Server resource.
func validateServer(server *klapv1alpha1.Server) (admission.Warnings, error) {
	var (
		allErrs     field.ErrorList
		allWarnings admission.Warnings
	)

	selector := server.Spec.AllowedNamespaces

	if selector != nil && selector.NamePattern != nil {
		_, err := regexp.Compile(fmt.Sprintf("^(?:%s)$", *selector.NamePattern))
		if err != nil {
			fieldErr := field.Invalid(field.NewPath("spec").Child("allowedNamespaces").Child("namePattern"), *selector.NamePattern, "must be a valid regular expression")
			allErrs = append(allErrs, fieldErr)
		}
	}

	if _, err := ldap.ParseDN(*server.Spec.BaseDN); err != nil {
		fieldErr := field.Invalid(field.NewPath("spec").Child("baseDN"), server.Spec.BaseDN, "must be a valid distinguished name")
		allErrs = append(allErrs, fieldErr)
	}

	if _, err := ldap.ParseDN(*server.Spec.BindDN); err != nil {
		fieldErr := field.Invalid(field.NewPath("spec").Child("bindDN"), server.Spec.BindDN, "must be a valid distinguished name")
		allErrs = append(allErrs, fieldErr)
	}

	if serverUrl, err := url.Parse(*server.Spec.Url); err != nil || !slices.Contains([]string{"ldap", "ldaps"}, serverUrl.Scheme) {
		fieldErr := field.Invalid(field.NewPath("spec").Child("url"), server.Spec.Url, "must be a valid LDAP URL")
		allErrs = append(allErrs, fieldErr)
	} else if serverUrl.Scheme == "ldap" && boolptr.IsSetToFalse(server.Spec.StartTLS) {
		allWarnings = append(allWarnings, "Using LDAP without StartTLS is not secure. Consider using LDAPS or enabling StartTLS.")
	}

	if len(allErrs) == 0 {
		return allWarnings, nil
	}

	return allWarnings, apierrors.NewInvalid(
		schema.GroupKind{Group: "klap.genesary.github.com", Kind: "Server"},
		server.Name, allErrs)
}
