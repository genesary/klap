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

package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/go-logr/logr"
	klapv1alpha1 "github.com/ripolin/klap/api/v1alpha1"
	"github.com/ripolin/klap/internal/util/boolptr"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Global constants
const (
	requeueAfterSuccess = 5 * time.Minute
	requeueFactor       = 0.2
	typeAvailable       = "Available"
)

// Active Directory constants
const (
	ActiveDirectory     = "activedirectory"
	ActiveDirectoryDN   = "distinguishedName"
	ActiveDirectoryGUID = "objectGUID"
)

// OpenLDAP constants
const (
	OpenLDAP     = "openldap"
	OpenLDAPDN   = "entryDN"
	OpenLDAPGUID = "entryUUID"
)

var Finalizer = fmt.Sprintf("%s/finalizer", klapv1alpha1.GroupVersion.Group)

// EntryReconciler reconciles a Entry object
type EntryReconciler struct {
	client.Client
	ldapClient ldap.Client
	Scheme     *runtime.Scheme
	Recorder   events.EventRecorder
}

// +kubebuilder:rbac:groups=klap.ripolin.github.com,resources=entries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=klap.ripolin.github.com,resources=entries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=klap.ripolin.github.com,resources=entries/finalizers,verbs=update
// +kubebuilder:rbac:groups=klap.ripolin.github.com,resources=servers,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/reconcile
func (r *EntryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var (
		cli    ldap.Client
		entry  = &klapv1alpha1.Entry{}
		tlsCfg = &tls.Config{}
		log    = logf.FromContext(ctx)
		opts   = []ldap.DialOpt{}
	)

	if cacert, err := x509.SystemCertPool(); err != nil {
		log.Error(err, "Unable to load system cacerts")
		tlsCfg.RootCAs = x509.NewCertPool()
	} else {
		tlsCfg.RootCAs = cacert
	}

	if err := r.Get(ctx, req.NamespacedName, entry); err != nil {
		if apierrs.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if entry.DeletionTimestamp != nil && boolptr.IsSetToFalse(entry.Spec.Prune) && controllerutil.RemoveFinalizer(entry, Finalizer) {
		if err := r.Update(ctx, entry); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	server, err := r.getServer(ctx, entry)

	if err != nil {
		return ctrl.Result{}, r.setStatusUnavailable(ctx, entry, err)
	}

	if err = r.checkEntryDN(entry, server); err != nil {
		return ctrl.Result{}, r.setStatusUnavailable(ctx, entry, err)
	}

	if server.Spec.TlsSecretRef.Name != nil {
		cacert, err := r.getCACert(ctx, server)
		if err != nil {
			return ctrl.Result{}, r.setStatusUnavailable(ctx, entry, err)
		}
		tlsCfg.RootCAs.AppendCertsFromPEM(cacert)
	}

	serverUrl, err := url.Parse(*server.Spec.Url)

	if err != nil {
		return ctrl.Result{}, r.setStatusUnavailable(ctx, entry, err)
	}

	tlsCfg.ServerName = serverUrl.Hostname()

	if serverUrl.Scheme == "ldaps" {
		opts = append(opts, ldap.DialWithTLSConfig(tlsCfg))
	}

	if r.ldapClient == nil {
		cli, err = ldap.DialURL(serverUrl.String(), opts...)
	} else {
		// For unit tests only !!!
		cli = r.ldapClient
	}

	if err != nil {
		return ctrl.Result{}, r.setStatusUnavailable(ctx, entry, err)
	}

	defer func() {
		if err := cli.Unbind(); err != nil {
			log.Error(err, err.Error())
		}
	}()

	cli.SetTimeout(server.Spec.Timeout.Duration)

	if boolptr.IsSetToTrue(server.Spec.StartTLS) && serverUrl.Scheme == "ldap" {
		if err = cli.StartTLS(tlsCfg); err != nil {
			return ctrl.Result{}, r.setStatusUnavailable(ctx, entry, err)
		}
	}

	password, err := r.getPassword(ctx, server)

	if err != nil {
		return ctrl.Result{}, r.setStatusUnavailable(ctx, entry, err)
	}

	err = cli.Bind(*server.Spec.BindDN, password)

	if err != nil {
		return ctrl.Result{}, r.setStatusUnavailable(ctx, entry, err)
	}

	if entry.DeletionTimestamp != nil && boolptr.IsSetToTrue(entry.Spec.Prune) {

		if err := r.deleteEntry(cli, entry, server, log); err != nil {
			return ctrl.Result{}, r.setStatusUnavailable(ctx, entry, err)
		} else {
			log.Info("Entry deleted successfully", "dn", entry.Spec.DN)
		}

		if controllerutil.RemoveFinalizer(entry, Finalizer) {
			if err := r.Update(ctx, entry); err != nil {
				return ctrl.Result{}, err
			}
		}

		return ctrl.Result{}, nil

	}

	if entry.Status.GUID != nil {
		err = r.updateEntry(cli, entry, server)
	} else {
		err = r.addEntry(cli, entry, server)
	}

	if err != nil {
		return ctrl.Result{}, r.setStatusUnavailable(ctx, entry, err)
	}

	if err = r.setStatusAvailable(ctx, entry); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: wait.Jitter(requeueAfterSuccess, requeueFactor)}, nil
}

// getServer retrieves the server configuration secret referenced by the Entry.
func (r *EntryReconciler) getServer(ctx context.Context, entry *klapv1alpha1.Entry) (*klapv1alpha1.Server, error) {
	var (
		server = &klapv1alpha1.Server{}
		ref    = &types.NamespacedName{
			Name:      *entry.Spec.ServerRef.Name,
			Namespace: *entry.Spec.ServerRef.Namespace,
		}
	)
	err := r.Get(ctx, *ref, server)
	if err != nil {
		return nil, err
	}

	allowed, err := r.isNamespaceAllowed(ctx, entry, server)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, fmt.Errorf(
			"namespace %q is not allowed to use server %s/%s",
			entry.Namespace, server.Namespace, server.Name)
	}

	return server, nil
}

// isNamespaceAllowed reports whether the Entry's namespace is allowed to use the
// referenced Server. An Entry living in the same namespace as the Server is
// always allowed. When the Server defines no AllowedNamespaces selector, only
// Entries from the Server's own namespace are allowed. Otherwise the Entry's
// namespace must match at least one configured criterion (name pattern or label
// selector).
func (r *EntryReconciler) isNamespaceAllowed(ctx context.Context, entry *klapv1alpha1.Entry, server *klapv1alpha1.Server) (bool, error) {
	// An Entry in the same namespace as the Server is always allowed.
	if entry.Namespace == server.Namespace {
		return true, nil
	}

	selector := server.Spec.AllowedNamespaces

	// No filtering configured: only the Server's own namespace is allowed.
	if selector == nil {
		return false, nil
	}

	// Match against the namespace name using the provided regular expression.
	if selector.NamePattern != nil {
		re, err := regexp.Compile(fmt.Sprintf("^(?:%s)$", *selector.NamePattern))
		if err != nil {
			return false, fmt.Errorf("invalid namespace name pattern %q: %w", *selector.NamePattern, err)
		}
		if re.MatchString(entry.Namespace) {
			return true, nil
		}
	}

	// Match against the namespace labels using the provided label selector.
	if selector.LabelSelector != nil {
		sel, err := metav1.LabelSelectorAsSelector(selector.LabelSelector)
		if err != nil {
			return false, fmt.Errorf("invalid namespace label selector: %w", err)
		}

		ns := &corev1.Namespace{}
		if err := r.Get(ctx, types.NamespacedName{Name: entry.Namespace}, ns); err != nil {
			return false, err
		}

		if sel.Matches(labels.Set(ns.Labels)) {
			return true, nil
		}
	}

	return false, nil
}

// getCACert retrieves the TLS certs bundle referenced by the Server.
func (r *EntryReconciler) getCACert(ctx context.Context, server *klapv1alpha1.Server) ([]byte, error) {
	var (
		secret = &corev1.Secret{}
		ref    = &types.NamespacedName{
			Name:      *server.Spec.TlsSecretRef.Name,
			Namespace: server.Namespace,
		}
	)
	err := r.Get(ctx, *ref, secret)
	if err != nil {
		return nil, err
	}
	if cacert, ok := secret.Data[*server.Spec.TlsSecretRef.Key]; ok {
		return cacert, nil
	}
	return nil, fmt.Errorf("%s key not found in secret %s", *server.Spec.TlsSecretRef.Key, ref.Name)
}

// getPassword retrieves the password secret referenced by the Server.
func (r *EntryReconciler) getPassword(ctx context.Context, server *klapv1alpha1.Server) (string, error) {
	var (
		secret = &corev1.Secret{}
		ref    = &types.NamespacedName{
			Name:      *server.Spec.PasswordSecretRef.Name,
			Namespace: server.Namespace,
		}
	)
	err := r.Get(ctx, *ref, secret)
	if err != nil {
		return "", err
	}
	if password, ok := secret.Data[*server.Spec.PasswordSecretRef.Key]; ok {
		return string(password), nil
	}
	return "", fmt.Errorf("%s key not found in secret %s", *server.Spec.PasswordSecretRef.Key, ref.Name)
}

// addEntry adds a new LDAP entry based on the provided Entry specification.
func (r *EntryReconciler) addEntry(cli ldap.Client, entry *klapv1alpha1.Entry, server *klapv1alpha1.Server) error {
	var (
		add    = ldap.NewAddRequest(*entry.Spec.DN, []ldap.Control{})
		search = ldap.NewSearchRequest(
			*server.Spec.BaseDN,
			ldap.ScopeWholeSubtree,
			ldap.NeverDerefAliases,
			0, 0, false,
			fmt.Sprintf("(%s=%s)", OpenLDAPDN, ldap.EscapeFilter(*entry.Spec.DN)),
			[]string{OpenLDAPGUID},
			nil,
		)
	)

	if *server.Spec.Implementation == ActiveDirectory {
		search.Filter = fmt.Sprintf("(%s=%s)", ActiveDirectoryDN, ldap.EscapeFilter(*entry.Spec.DN))
		search.Attributes = []string{ActiveDirectoryGUID}
	}

	for k, v := range entry.Spec.Attributes {
		add.Attributes = append(add.Attributes, ldap.Attribute{
			Type: k,
			Vals: v,
		})
	}

	if err := cli.Add(add); err != nil {
		if !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) || boolptr.IsSetToFalse(entry.Spec.Adopt) {
			return err
		}
	}

	if searchResult, err := cli.Search(search); err != nil {
		return err
	} else {
		guidAttr := OpenLDAPGUID
		if *server.Spec.Implementation == ActiveDirectory {
			guidAttr = ActiveDirectoryGUID
		}
		guid := searchResult.Entries[0].GetAttributeValue(guidAttr)
		if guid == "" {
			return fmt.Errorf("unable to retrieve entry GUID")
		}
		entry.Status.GUID = &guid
	}

	return nil
}

// updateEntry updates an existing LDAP entry based on the provided Entry specification.
func (r *EntryReconciler) updateEntry(cli ldap.Client, entry *klapv1alpha1.Entry, server *klapv1alpha1.Server) error {
	var (
		modify = ldap.NewModifyRequest(*entry.Spec.DN, []ldap.Control{})
		search = ldap.NewSearchRequest(
			*server.Spec.BaseDN,
			ldap.ScopeWholeSubtree,
			ldap.NeverDerefAliases,
			0, 0, false,
			fmt.Sprintf("(%s=%s)", OpenLDAPGUID, ldap.EscapeFilter(*entry.Status.GUID)),
			[]string{"*"},
			nil,
		)
	)

	if *server.Spec.Implementation == ActiveDirectory {
		search.Filter = fmt.Sprintf("(%s=%s)", ActiveDirectoryGUID, ldap.EscapeFilter(*entry.Status.GUID))
	}

	if searchResult, err := cli.Search(search); err != nil {
		return err
	} else {

		if len(searchResult.Entries) == 0 {
			guid := *entry.Status.GUID
			entry.Status.GUID = nil
			return fmt.Errorf("entry %s not found", guid)
		}

		current := searchResult.Entries[0]
		dn, _ := ldap.ParseDN(*entry.Spec.DN)

		if current.DN != *entry.Spec.DN {
			newSup := &ldap.DN{
				RDNs: []*ldap.RelativeDN{},
			}
			newSup.RDNs = append(newSup.RDNs, dn.RDNs[1:]...)
			moddn := ldap.NewModifyDNRequest(
				current.DN,
				dn.RDNs[0].String(),
				true,
				newSup.String(),
			)
			if err = cli.ModifyDN(moddn); err != nil {
				return err
			}
		}

		for k, v := range entry.Spec.Attributes {
			if len(current.GetAttributeValues(k)) == 0 {
				modify.Add(k, v)
				continue
			}
			if boolptr.IsSetToTrue(entry.Spec.Force) {
				if slices.Compare(v, current.GetAttributeValues(k)) != 0 {
					modify.Replace(k, v)
				}
			} else {
				for _, val := range v {
					if !slices.Contains(current.GetAttributeValues(k), val) {
						modify.Add(k, []string{val})
					}
				}
			}
		}

		if boolptr.IsSetToTrue(entry.Spec.Force) {
			for _, attr := range current.Attributes {

				// Skip the RDN attribute computed from the DN
				if attr.Name == dn.RDNs[0].Attributes[0].Type {
					continue
				}

				if _, ok := entry.Spec.Attributes[attr.Name]; !ok {
					modify.Delete(attr.Name, attr.Values)
				}
			}
		}
	}

	if len(modify.Changes) > 0 {
		return cli.Modify(modify)
	}

	return nil
}

// deleteEntry deletes an existing LDAP entry based on the provided Entry specification.
func (r *EntryReconciler) deleteEntry(cli ldap.Client, entry *klapv1alpha1.Entry, server *klapv1alpha1.Server, log logr.Logger) error {

	if entry.Status.GUID == nil {
		log.Info("Entry has no GUID, skipping deletion", "dn", entry.Spec.DN)
		return nil
	}

	var (
		delete = ldap.NewDelRequest(*entry.Spec.DN, []ldap.Control{})
		search = ldap.NewSearchRequest(
			*server.Spec.BaseDN,
			ldap.ScopeWholeSubtree,
			ldap.NeverDerefAliases,
			0, 0, false,
			fmt.Sprintf("(%s=%s)", OpenLDAPGUID, ldap.EscapeFilter(*entry.Status.GUID)),
			[]string{"*"},
			nil,
		)
	)

	if *server.Spec.Implementation == ActiveDirectory {
		search.Filter = fmt.Sprintf("(%s=%s)", ActiveDirectoryGUID, ldap.EscapeFilter(*entry.Status.GUID))
	}

	if searchResult, err := cli.Search(search); err != nil {
		return err
	} else {
		if len(searchResult.Entries) == 1 {
			if searchResult.Entries[0].DN == *entry.Spec.DN {
				return cli.Del(delete)
			} else {
				return fmt.Errorf("entry %s has a different DN than expected: %s", *entry.Status.GUID, searchResult.Entries[0].DN)
			}
		} else {
			log.Info("Entry not found, skipping deletion", "guid", *entry.Status.GUID)
			return nil
		}
	}
}

// checkEntryDN checks if the Entry's DN is a descendant of the Server's BaseDN.
func (r *EntryReconciler) checkEntryDN(entry *klapv1alpha1.Entry, server *klapv1alpha1.Server) error {
	baseDN, err := ldap.ParseDN(*server.Spec.BaseDN)
	if err != nil {
		return err
	}

	entryDN, err := ldap.ParseDN(*entry.Spec.DN)
	if err != nil {
		return err
	}

	if !baseDN.AncestorOf(entryDN) {
		return fmt.Errorf("%s is not a descendant of %s", *entry.Spec.DN, *server.Spec.BaseDN)
	}

	return nil
}

// setStatusAvailable updates the Entry status to Available.
func (r *EntryReconciler) setStatusAvailable(ctx context.Context, entry *klapv1alpha1.Entry) error {

	if meta.SetStatusCondition(&entry.Status.Conditions, metav1.Condition{
		Type:               typeAvailable,
		Status:             metav1.ConditionTrue,
		Reason:             "ReconciledSuccessfully",
		Message:            "Entry is reconciled successfully",
		ObservedGeneration: entry.Generation,
	}) {
		r.Recorder.Eventf(entry, nil, "Normal", "EntryDriftDetection", "Reconcile", "entry %s successfully reconciled", *entry.Spec.DN)
		return r.Status().Update(ctx, entry)
	}

	return nil
}

// setStatusUnavailable updates the Entry status to Unavailable with the provided error message.
func (r *EntryReconciler) setStatusUnavailable(ctx context.Context, entry *klapv1alpha1.Entry, err error) error {

	r.Recorder.Eventf(entry, nil, "Warning", "ErrorOccurs", "Reconcile", err.Error())

	status := metav1.ConditionFalse

	if entry.Status.GUID != nil {
		// If the entry has a GUID, it means it was previously created.
		// In this case, we set the status to Unknown to indicate that
		// the current state is uncertain due to the error.
		status = metav1.ConditionUnknown
	}

	if meta.SetStatusCondition(&entry.Status.Conditions, metav1.Condition{
		Type:               typeAvailable,
		Status:             status,
		Reason:             "ErrorOccurred",
		Message:            err.Error(),
		ObservedGeneration: entry.Generation,
	}) {
		return r.Status().Update(ctx, entry)
	}

	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *EntryReconciler) SetupWithManager(mgr ctrl.Manager, maxConcurrentReconciles int) error {

	rateLimiter := workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](
			10*time.Second,
			5*time.Minute,
		),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{
			Limiter: rate.NewLimiter(rate.Limit(10), 100),
		},
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(&klapv1alpha1.Entry{}).
		Named("entry").
		WithOptions(controller.Options{
			MaxConcurrentReconciles: maxConcurrentReconciles,
			RateLimiter:             rateLimiter,
		}).
		Complete(r)
}
