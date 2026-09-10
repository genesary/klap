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
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/reconcile
func (r *EntryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var (
		entry = &klapv1alpha1.Entry{}
		log   = logf.FromContext(ctx)
	)

	if err := r.Get(ctx, req.NamespacedName, entry); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The entry is being deleted but must be left untouched on the server:
	// release the finalizer without connecting to it.
	if entry.DeletionTimestamp != nil && boolptr.IsSetToFalse(entry.Spec.Prune) && controllerutil.RemoveFinalizer(entry, Finalizer) {
		return ctrl.Result{}, r.Update(ctx, entry)
	}

	server, err := r.getServer(ctx, entry)

	if err != nil {
		return ctrl.Result{}, r.setStatusUnavailable(ctx, entry, err)
	}

	if err = r.checkEntryDN(entry, server); err != nil {
		return ctrl.Result{}, r.setStatusUnavailable(ctx, entry, err)
	}

	cli, err := r.connect(ctx, server)

	if err != nil {
		return ctrl.Result{}, r.setStatusUnavailable(ctx, entry, err)
	}

	defer r.unbind(cli, log)

	if entry.DeletionTimestamp != nil && boolptr.IsSetToTrue(entry.Spec.Prune) {
		return ctrl.Result{}, r.pruneEntry(ctx, cli, entry, server)
	}

	if err = r.syncEntry(cli, entry, server); err != nil {
		return ctrl.Result{}, r.setStatusUnavailable(ctx, entry, err)
	}

	if err = r.setStatusAvailable(ctx, entry); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: wait.Jitter(requeueAfterSuccess, requeueFactor)}, nil
}

// unbind closes the LDAP connection, logging any error raised while doing so.
func (r *EntryReconciler) unbind(cli ldap.Client, log logr.Logger) {
	if err := cli.Unbind(); err != nil {
		log.Error(err, err.Error())
	}
}

// tlsConfig builds the TLS configuration used to reach the server, seeded with the
// system CA pool and completed with the CA certificate referenced by the Server, if any.
func (r *EntryReconciler) tlsConfig(ctx context.Context, server *klapv1alpha1.Server) (*tls.Config, error) {
	tlsCfg := &tls.Config{}

	if cacerts, err := x509.SystemCertPool(); err != nil {
		logf.FromContext(ctx).Error(err, "Unable to load system cacerts")
		tlsCfg.RootCAs = x509.NewCertPool()
	} else {
		tlsCfg.RootCAs = cacerts
	}

	if server.Spec.TlsSecretRef.Name == nil {
		return tlsCfg, nil
	}

	cacerts, err := r.getCACerts(ctx, server)
	if err != nil {
		return nil, err
	}

	if !tlsCfg.RootCAs.AppendCertsFromPEM(cacerts) {
		logf.FromContext(ctx).Error(err, "Unable to append cacert to the root CA pool")
	}

	return tlsCfg, nil
}

// dial opens the connection to the LDAP server.
func (r *EntryReconciler) dial(serverUrl *url.URL, tlsCfg *tls.Config) (ldap.Client, error) {
	if r.ldapClient != nil {
		// For unit tests only !!!
		return r.ldapClient, nil
	}

	opts := []ldap.DialOpt{}

	if serverUrl.Scheme == "ldaps" {
		opts = append(opts, ldap.DialWithTLSConfig(tlsCfg))
	}

	return ldap.DialURL(serverUrl.String(), opts...)
}

// connect dials the LDAP server, negotiates StartTLS when requested and binds
// with the credentials referenced by the Server. The returned client is bound
// and must be released by the caller; it is released here on failure.
func (r *EntryReconciler) connect(ctx context.Context, server *klapv1alpha1.Server) (ldap.Client, error) {
	tlsCfg, err := r.tlsConfig(ctx, server)
	if err != nil {
		return nil, err
	}

	serverUrl, err := url.Parse(*server.Spec.Url)
	if err != nil {
		return nil, err
	}

	tlsCfg.ServerName = serverUrl.Hostname()

	cli, err := r.dial(serverUrl, tlsCfg)
	if err != nil {
		return nil, err
	}

	// Only the error is a named return: cli stays valid here even though the
	// failure paths below return a nil client to the caller.
	defer func() {
		if err != nil {
			r.unbind(cli, logf.FromContext(ctx))
		}
	}()

	cli.SetTimeout(server.Spec.Timeout.Duration)

	if boolptr.IsSetToTrue(server.Spec.StartTLS) && serverUrl.Scheme == "ldap" {
		if err = cli.StartTLS(tlsCfg); err != nil {
			return nil, err
		}
	}

	password, err := r.getPassword(ctx, server)
	if err != nil {
		return nil, err
	}

	if err = cli.Bind(*server.Spec.BindDN, password); err != nil {
		return nil, err
	}

	return cli, nil
}

// pruneEntry deletes the LDAP entry backing an Entry being deleted, then releases its finalizer.
func (r *EntryReconciler) pruneEntry(ctx context.Context, cli ldap.Client, entry *klapv1alpha1.Entry, server *klapv1alpha1.Server) error {
	log := logf.FromContext(ctx)

	if err := r.deleteEntry(cli, entry, server, log); err != nil {
		return r.setStatusUnavailable(ctx, entry, err)
	}

	log.Info("Entry deleted successfully", "dn", entry.Spec.DN)

	if controllerutil.RemoveFinalizer(entry, Finalizer) {
		return r.Update(ctx, entry)
	}

	return nil
}

// syncEntry adds the LDAP entry, or updates it once it has been created.
func (r *EntryReconciler) syncEntry(cli ldap.Client, entry *klapv1alpha1.Entry, server *klapv1alpha1.Server) error {
	if entry.Status.GUID != nil {
		return r.updateEntry(cli, entry, server)
	}

	return r.addEntry(cli, entry, server)
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

// getCACerts retrieves the TLS certs bundle referenced by the Server.
func (r *EntryReconciler) getCACerts(ctx context.Context, server *klapv1alpha1.Server) ([]byte, error) {
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
	add := ldap.NewAddRequest(*entry.Spec.DN, []ldap.Control{})

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

	search := ldap.NewSearchRequest(
		*server.Spec.BaseDN,
		ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases,
		0, 0, false,
		fmt.Sprintf("(%s=%s)", OpenLDAPDN, ldap.EscapeFilter(*entry.Spec.DN)),
		[]string{OpenLDAPGUID},
		nil,
	)

	guidAttr := OpenLDAPGUID

	if *server.Spec.Implementation == ActiveDirectory {
		search.Filter = fmt.Sprintf("(%s=%s)", ActiveDirectoryDN, ldap.EscapeFilter(*entry.Spec.DN))
		search.Attributes = []string{ActiveDirectoryGUID}
		guidAttr = ActiveDirectoryGUID
	}

	searchResult, err := cli.Search(search)

	if err != nil {
		return err
	}

	guid := searchResult.Entries[0].GetAttributeValue(guidAttr)
	if guid == "" {
		return fmt.Errorf("unable to retrieve entry GUID")
	}
	entry.Status.GUID = &guid

	return nil
}

// searchEntryByGUID looks up an LDAP entry by the GUID stored in the Entry status,
// using the GUID attribute appropriate for the server implementation.
func (r *EntryReconciler) searchEntryByGUID(cli ldap.Client, entry *klapv1alpha1.Entry, server *klapv1alpha1.Server) (*ldap.SearchResult, error) {
	guidAttr := OpenLDAPGUID
	if *server.Spec.Implementation == ActiveDirectory {
		guidAttr = ActiveDirectoryGUID
	}

	search := ldap.NewSearchRequest(
		*server.Spec.BaseDN,
		ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases,
		0, 0, false,
		fmt.Sprintf("(%s=%s)", guidAttr, ldap.EscapeFilter(*entry.Status.GUID)),
		[]string{"*"},
		nil,
	)

	return cli.Search(search)
}

// renameEntryIfMoved issues a ModifyDN request when the entry's current DN
// differs from the DN expected by the Entry specification.
func (r *EntryReconciler) renameEntryIfMoved(cli ldap.Client, current *ldap.Entry, entry *klapv1alpha1.Entry, dn *ldap.DN) error {
	if current.DN == *entry.Spec.DN {
		return nil
	}

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

	return cli.ModifyDN(moddn)
}

// applyAttributeAdditions stages additions for attributes missing from the current
// entry, and, depending on Force, either replaces or merges attributes already present.
func (r *EntryReconciler) applyAttributeAdditions(modify *ldap.ModifyRequest, current *ldap.Entry, entry *klapv1alpha1.Entry) {
	force := boolptr.IsSetToTrue(entry.Spec.Force)

	for k, v := range entry.Spec.Attributes {
		currentValues := current.GetAttributeValues(k)

		if len(currentValues) == 0 {
			modify.Add(k, v)
			continue
		}

		if force {
			if slices.Compare(v, currentValues) != 0 {
				modify.Replace(k, v)
			}
			continue
		}

		for _, val := range v {
			if !slices.Contains(currentValues, val) {
				modify.Add(k, []string{val})
			}
		}
	}
}

// applyAttributeDeletions stages deletions for attributes present on the current entry
// but absent from the specification. It is a no-op unless Force is enabled.
func (r *EntryReconciler) applyAttributeDeletions(modify *ldap.ModifyRequest, current *ldap.Entry, entry *klapv1alpha1.Entry, dn *ldap.DN) {
	if !boolptr.IsSetToTrue(entry.Spec.Force) {
		return
	}

	rdnAttr := dn.RDNs[0].Attributes[0].Type

	for _, attr := range current.Attributes {
		// Skip the RDN attribute computed from the DN
		if attr.Name == rdnAttr {
			continue
		}

		if _, ok := entry.Spec.Attributes[attr.Name]; !ok {
			modify.Delete(attr.Name, attr.Values)
		}
	}
}

// updateEntry updates an existing LDAP entry based on the provided Entry specification.
func (r *EntryReconciler) updateEntry(cli ldap.Client, entry *klapv1alpha1.Entry, server *klapv1alpha1.Server) error {
	searchResult, err := r.searchEntryByGUID(cli, entry, server)
	if err != nil {
		return err
	}

	if len(searchResult.Entries) == 0 {
		guid := *entry.Status.GUID
		entry.Status.GUID = nil
		return fmt.Errorf("entry %s not found", guid)
	}

	current := searchResult.Entries[0]
	dn, err := ldap.ParseDN(*entry.Spec.DN)

	if err != nil {
		return err
	}

	if err := r.renameEntryIfMoved(cli, current, entry, dn); err != nil {
		return err
	}

	modify := ldap.NewModifyRequest(*entry.Spec.DN, []ldap.Control{})
	r.applyAttributeAdditions(modify, current, entry)
	r.applyAttributeDeletions(modify, current, entry, dn)

	if len(modify.Changes) == 0 {
		return nil
	}

	return cli.Modify(modify)
}

// deleteEntry deletes an existing LDAP entry based on the provided Entry specification.
func (r *EntryReconciler) deleteEntry(cli ldap.Client, entry *klapv1alpha1.Entry, server *klapv1alpha1.Server, log logr.Logger) error {

	if entry.Status.GUID == nil {
		log.Info("Entry has no GUID, skipping deletion", "dn", entry.Spec.DN)
		return nil
	}

	searchResult, err := r.searchEntryByGUID(cli, entry, server)

	if err != nil {
		return err
	}

	if len(searchResult.Entries) == 1 {
		if searchResult.Entries[0].DN == *entry.Spec.DN {
			delete := ldap.NewDelRequest(*entry.Spec.DN, []ldap.Control{})
			return cli.Del(delete)
		}
		return fmt.Errorf("entry %s has a different DN than expected: %s", *entry.Status.GUID, searchResult.Entries[0].DN)
	}

	log.Info("Entry not found, skipping deletion", "guid", *entry.Status.GUID)
	return nil
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
