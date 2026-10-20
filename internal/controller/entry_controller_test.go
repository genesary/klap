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
	"fmt"

	klapv1alpha1 "github.com/genesary/klap/api/v1alpha1"
	"github.com/genesary/klap/internal/util/boolptr"
	"github.com/genesary/klap/test/mock_ldap"
	"github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	gomock "go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// noSuchObject mimics the error an LDAP server returns when a base search
// targets a DN absent from the directory.
var noSuchObject = ldap.NewError(ldap.LDAPResultNoSuchObject, fmt.Errorf("no such object"))

const (
	objectClass        = "objectClass"
	groupOfNames       = "groupOfNames"
	organizationalUnit = "organizationalUnit"
	description        = "description"
)

var _ = Describe("Entry Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-entry"
		const serverName = "my-ldap-server"
		const passwdName = "my-passwd"
		const uuid = "fab03ddc-1989-471d-84f3-4c19d32fda35"

		var ns = "default"
		var dn = "cn=foo,dc=bar"

		var mockClient *mock_ldap.MockClient

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: ns,
		}

		serverTypeNamespacedName := types.NamespacedName{
			Name:      serverName,
			Namespace: ns,
		}

		passwdTypeNamespacedName := types.NamespacedName{
			Name:      passwdName,
			Namespace: ns,
		}

		entry := &klapv1alpha1.Entry{}
		server := &klapv1alpha1.Server{}
		passwd := &corev1.Secret{}

		BeforeEach(func() {
			mockCtrl := gomock.NewController(GinkgoT())
			mockClient = mock_ldap.NewMockClient(mockCtrl)
			mockClient.EXPECT().SetTimeout(gomock.Any()).AnyTimes()

			By("creating the custom resource for the Kind Secret")
			err := k8sClient.Get(ctx, passwdTypeNamespacedName, passwd)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      passwdName,
						Namespace: ns,
					},
					StringData: map[string]string{
						"password": "foobar",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}

			By("creating the custom resource for the Kind Server")
			err = k8sClient.Get(ctx, serverTypeNamespacedName, server)
			if err != nil && errors.IsNotFound(err) {
				baseDN := "dc=bar"
				bindDN := "cn=admin,dc=bar"
				ldapUrl := "http://my-ldap-server"
				passwordName := passwdName
				key := "password"
				resource := &klapv1alpha1.Server{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serverName,
						Namespace: ns,
					},
					Spec: klapv1alpha1.ServerSpec{
						BaseDN: &baseDN,
						BindDN: &bindDN,
						Url:    &ldapUrl,
						PasswordSecretRef: klapv1alpha1.SecretRef{
							Name: &passwordName,
							Key:  &key,
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}

			By("creating the custom resource for the Kind Entry")
			err = k8sClient.Get(ctx, typeNamespacedName, entry)
			if err != nil && errors.IsNotFound(err) {
				sName := serverName
				resource := &klapv1alpha1.Entry{
					ObjectMeta: metav1.ObjectMeta{
						Name:       resourceName,
						Namespace:  ns,
						Finalizers: []string{Finalizer},
					},
					Spec: klapv1alpha1.EntrySpec{
						DN:    &dn,
						Prune: boolptr.True(),
						Force: boolptr.False(),
						Attributes: map[string][]string{
							objectClass: {groupOfNames},
							description: {"test"},
						},
						ServerRef: klapv1alpha1.ResourceRef{
							Name:      &sName,
							Namespace: &ns,
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			// resource := &klapv1alpha1.Entry{}
			// err := k8sClient.Get(ctx, typeNamespacedName, resource)
			// Expect(err).NotTo(HaveOccurred())

			// By("Cleanup the specific resource instance Entry")
			// Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			serverResource := &klapv1alpha1.Server{}
			err := k8sClient.Get(ctx, serverTypeNamespacedName, serverResource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Server")
			Expect(k8sClient.Delete(ctx, serverResource)).To(Succeed())

			passwdResource := &corev1.Secret{}
			err = k8sClient.Get(ctx, passwdTypeNamespacedName, passwdResource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Secret")
			Expect(k8sClient.Delete(ctx, passwdResource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")

			mockClient.EXPECT().Bind(gomock.Any(), gomock.Any()).MaxTimes(3).Return(nil)
			searchByDN := gomock.Cond(func(search *ldap.SearchRequest) bool {
				return search.BaseDN == dn && search.Scope == ldap.ScopeBaseObject
			})
			gomock.InOrder(
				mockClient.EXPECT().Search(searchByDN).Return(nil, noSuchObject),
				mockClient.EXPECT().Add(gomock.Any()).Return(nil),
				mockClient.EXPECT().Search(searchByDN).Return(&ldap.SearchResult{
					Entries: []*ldap.Entry{
						{
							DN: dn,
							Attributes: []*ldap.EntryAttribute{
								{
									Name:   OpenLDAPGUID,
									Values: []string{uuid},
								},
							},
						},
					},
				}, nil),
			)
			mockClient.EXPECT().Unbind().MaxTimes(3).Return(nil)

			controllerReconciler := &EntryReconciler{
				ldapClient: mockClient,
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				Recorder:   recorder,
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
			entry := &klapv1alpha1.Entry{}
			err = k8sClient.Get(ctx, typeNamespacedName, entry)
			Expect(err).NotTo(HaveOccurred())
			Expect(entry.Status.GUID).NotTo(BeNil())

			By("Reconciling the existing resource")
			mockClient.EXPECT().Search(gomock.Cond(func(search *ldap.SearchRequest) bool {
				return search.Filter == fmt.Sprintf("(%s=%s)", OpenLDAPGUID, uuid)
			})).Return(&ldap.SearchResult{
				Entries: []*ldap.Entry{
					{
						DN: dn,
						Attributes: []*ldap.EntryAttribute{
							{
								Name:   objectClass,
								Values: []string{groupOfNames},
							},
							{
								Name:   description,
								Values: []string{"changed"},
							},
						},
					},
				},
			}, nil)
			mockClient.EXPECT().Modify(gomock.Any()).Return(nil)
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Deleting the existing resource")
			mockClient.EXPECT().Search(gomock.Cond(func(search *ldap.SearchRequest) bool {
				return search.Filter == fmt.Sprintf("(%s=%s)", OpenLDAPGUID, uuid)
			})).Return(&ldap.SearchResult{
				Entries: []*ldap.Entry{
					{
						DN: dn,
					},
				},
			}, nil)
			mockClient.EXPECT().Del(gomock.Cond(func(delete *ldap.DelRequest) bool {
				return delete.DN == dn
			})).Return(nil)
			err = k8sClient.Get(ctx, typeNamespacedName, entry)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, entry)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should fail when the entry DN is outside the server BaseDN", func() {
			By("Creating an entry whose DN does not match the server BaseDN")
			// The shared Server uses baseDN "foo"; this DN does not contain it.
			badDN := "cn=outside,dc=elsewhere,dc=org"
			badName := types.NamespacedName{Name: "bad-dn-entry", Namespace: ns}
			sName := serverName
			Expect(k8sClient.Create(ctx, &klapv1alpha1.Entry{
				ObjectMeta: metav1.ObjectMeta{
					Name:      badName.Name,
					Namespace: badName.Namespace,
				},
				Spec: klapv1alpha1.EntrySpec{
					DN:         &badDN,
					Prune:      boolptr.False(),
					Force:      boolptr.False(),
					Attributes: map[string][]string{objectClass: {groupOfNames}},
					ServerRef: klapv1alpha1.ResourceRef{
						Name:      &sName,
						Namespace: &ns,
					},
				},
			})).To(Succeed())

			controllerReconciler := &EntryReconciler{
				ldapClient: mockClient,
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				Recorder:   recorder,
			}

			By("Reconciling the mismatching entry")
			// The BaseDN check runs before any LDAP dial/bind, so the mock must
			// observe no Bind/Add/Search call; gomock would fail on an unexpected one.
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: badName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the entry is marked Unavailable with a BaseDN error")
			result := &klapv1alpha1.Entry{}
			Expect(k8sClient.Get(ctx, badName, result)).To(Succeed())
			cond := meta.FindStatusCondition(result.Status.Conditions, typeAvailable)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Message).To(ContainSubstring("is not a descendant of"))

			Expect(k8sClient.Delete(ctx, result)).To(Succeed())
		})

		It("should release the connection when the bind fails", func() {
			By("Making the LDAP bind fail")
			// The connection is already open when the bind runs, so the failure must
			// still unbind it. Unbinding a nil client here would panic instead.
			mockClient.EXPECT().Bind(gomock.Any(), gomock.Any()).Return(fmt.Errorf("invalid credentials"))
			mockClient.EXPECT().Unbind().Times(1).Return(nil)

			controllerReconciler := &EntryReconciler{
				ldapClient: mockClient,
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				Recorder:   recorder,
			}

			By("Reconciling the resource")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the entry reports the bind error")
			result := &klapv1alpha1.Entry{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, result)).To(Succeed())
			cond := meta.FindStatusCondition(result.Status.Conditions, typeAvailable)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Message).To(ContainSubstring("invalid credentials"))
		})
	})

	Context("When filtering entries by namespace", func() {
		const serverNamespace = "server-ns"
		const teamLabel = "team"
		const teamBlue = "blue"

		ctx := context.Background()

		var reconciler *EntryReconciler

		// newServer builds a Server living in serverNamespace with the given selector.
		newServer := func(selector *klapv1alpha1.NamespaceSelector) *klapv1alpha1.Server {
			return &klapv1alpha1.Server{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "filtered-server",
					Namespace: serverNamespace,
				},
				Spec: klapv1alpha1.ServerSpec{
					AllowedNamespaces: selector,
				},
			}
		}

		// newEntry builds an Entry living in the given namespace.
		newEntry := func(namespace string) *klapv1alpha1.Entry {
			return &klapv1alpha1.Entry{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "filtered-entry",
					Namespace: namespace,
				},
			}
		}

		ptr := func(s string) *string { return &s }

		BeforeEach(func() {
			reconciler = &EntryReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: recorder,
			}

			By("creating namespaces used by the label selector cases")
			for name, lbls := range map[string]map[string]string{
				"team-blue":   {teamLabel: teamBlue},
				"team-red":    {teamLabel: "red"},
				"no-label-ns": nil,
			} {
				ns := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   name,
						Labels: lbls,
					},
				}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, ns)
				if err != nil && errors.IsNotFound(err) {
					Expect(k8sClient.Create(ctx, ns)).To(Succeed())
				}
			}
		})

		It("allows an entry living in the same namespace as the server", func() {
			// Even with a selector that matches nothing, same-namespace is allowed.
			server := newServer(&klapv1alpha1.NamespaceSelector{NamePattern: ptr("nomatch")})
			entry := newEntry(serverNamespace)

			allowed, err := reconciler.isNamespaceAllowed(ctx, entry, server)
			Expect(err).NotTo(HaveOccurred())
			Expect(allowed).To(BeTrue())
		})

		It("denies a foreign namespace when no selector is configured", func() {
			server := newServer(nil)
			entry := newEntry("team-blue")

			allowed, err := reconciler.isNamespaceAllowed(ctx, entry, server)
			Expect(err).NotTo(HaveOccurred())
			Expect(allowed).To(BeFalse())
		})

		It("allows the server's own namespace when no selector is configured", func() {
			server := newServer(nil)
			entry := newEntry(serverNamespace)

			allowed, err := reconciler.isNamespaceAllowed(ctx, entry, server)
			Expect(err).NotTo(HaveOccurred())
			Expect(allowed).To(BeTrue())
		})

		It("allows an entry whose namespace name matches the pattern", func() {
			server := newServer(&klapv1alpha1.NamespaceSelector{NamePattern: ptr("team-.*")})
			entry := newEntry("team-blue")

			allowed, err := reconciler.isNamespaceAllowed(ctx, entry, server)
			Expect(err).NotTo(HaveOccurred())
			Expect(allowed).To(BeTrue())
		})

		It("denies an entry whose namespace name does not match the pattern", func() {
			server := newServer(&klapv1alpha1.NamespaceSelector{NamePattern: ptr("team-.*")})
			entry := newEntry("no-label-ns")

			allowed, err := reconciler.isNamespaceAllowed(ctx, entry, server)
			Expect(err).NotTo(HaveOccurred())
			Expect(allowed).To(BeFalse())
		})

		It("anchors the pattern to the full namespace name", func() {
			// "team" must not match "team-blue" since the pattern is anchored.
			server := newServer(&klapv1alpha1.NamespaceSelector{NamePattern: ptr("team")})
			entry := newEntry("team-blue")

			allowed, err := reconciler.isNamespaceAllowed(ctx, entry, server)
			Expect(err).NotTo(HaveOccurred())
			Expect(allowed).To(BeFalse())
		})

		It("allows an entry whose namespace labels match the selector", func() {
			server := newServer(&klapv1alpha1.NamespaceSelector{
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{teamLabel: teamBlue},
				},
			})
			entry := newEntry("team-blue")

			allowed, err := reconciler.isNamespaceAllowed(ctx, entry, server)
			Expect(err).NotTo(HaveOccurred())
			Expect(allowed).To(BeTrue())
		})

		It("denies an entry whose namespace labels do not match the selector", func() {
			server := newServer(&klapv1alpha1.NamespaceSelector{
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{teamLabel: teamBlue},
				},
			})
			entry := newEntry("team-red")

			allowed, err := reconciler.isNamespaceAllowed(ctx, entry, server)
			Expect(err).NotTo(HaveOccurred())
			Expect(allowed).To(BeFalse())
		})
	})

	Context("When adding an entry", func() {
		const (
			existingDN   = "cn=existing,dc=example,dc=org"
			existingUUID = "3a5e9b2c-0000-4a1b-8c2d-1122334455ff"
		)

		var (
			mockClient *mock_ldap.MockClient
			reconciler *EntryReconciler
			server     *klapv1alpha1.Server
		)

		// newEntry builds an Entry targeting existingDN with the given adopt policy.
		newEntry := func(adopt *bool) *klapv1alpha1.Entry {
			dn := existingDN
			return &klapv1alpha1.Entry{
				Spec: klapv1alpha1.EntrySpec{
					DN:    &dn,
					Adopt: adopt,
					Attributes: map[string][]string{
						objectClass: {organizationalUnit},
					},
				},
			}
		}

		// searchByDN matches the base search reading existingDN.
		searchByDN := gomock.Cond(func(search *ldap.SearchRequest) bool {
			return search.BaseDN == existingDN && search.Scope == ldap.ScopeBaseObject
		})

		// found is the result of a search reading the entry at existingDN.
		found := &ldap.SearchResult{
			Entries: []*ldap.Entry{
				{
					DN: existingDN,
					Attributes: []*ldap.EntryAttribute{
						{Name: OpenLDAPGUID, Values: []string{existingUUID}},
					},
				},
			},
		}

		BeforeEach(func() {
			mockCtrl := gomock.NewController(GinkgoT())
			mockClient = mock_ldap.NewMockClient(mockCtrl)
			reconciler = &EntryReconciler{}

			baseDN := "dc=example,dc=org"
			impl := OpenLDAP
			server = &klapv1alpha1.Server{
				Spec: klapv1alpha1.ServerSpec{
					BaseDN:         &baseDN,
					Implementation: &impl,
				},
			}
		})

		It("adds the entry when it does not exist", func() {
			gomock.InOrder(
				mockClient.EXPECT().Search(searchByDN).Return(nil, noSuchObject),
				mockClient.EXPECT().Add(gomock.Cond(func(add *ldap.AddRequest) bool {
					return add.DN == existingDN
				})).Return(nil),
				mockClient.EXPECT().Search(searchByDN).Return(found, nil),
			)

			entry := newEntry(boolptr.True())
			Expect(reconciler.addEntry(mockClient, entry, server)).To(Succeed())
			Expect(entry.Status.GUID).NotTo(BeNil())
			Expect(*entry.Status.GUID).To(Equal(existingUUID))
		})

		It("fails when the added entry cannot be read back", func() {
			gomock.InOrder(
				mockClient.EXPECT().Search(searchByDN).Return(nil, noSuchObject),
				mockClient.EXPECT().Add(gomock.Any()).Return(nil),
				mockClient.EXPECT().Search(searchByDN).Return(nil, noSuchObject),
			)

			entry := newEntry(boolptr.True())
			Expect(reconciler.addEntry(mockClient, entry, server)).To(MatchError(ContainSubstring("not found after its addition")))
			Expect(entry.Status.GUID).To(BeNil())
		})

		It("surfaces a search failure without adding the entry", func() {
			// No Add is expected: gomock would fail on an unexpected call.
			mockClient.EXPECT().Search(searchByDN).Return(nil, ldap.NewError(ldap.LDAPResultInsufficientAccessRights, fmt.Errorf("access denied")))

			entry := newEntry(boolptr.True())
			err := reconciler.addEntry(mockClient, entry, server)
			Expect(ldap.IsErrorWithCode(err, ldap.LDAPResultInsufficientAccessRights)).To(BeTrue())
			Expect(entry.Status.GUID).To(BeNil())
		})

		It("adopts the existing entry when adopt is true", func() {
			// No Add is expected: the entry is already present.
			mockClient.EXPECT().Search(searchByDN).Return(found, nil)

			entry := newEntry(boolptr.True())
			Expect(reconciler.addEntry(mockClient, entry, server)).To(Succeed())
			Expect(entry.Status.GUID).NotTo(BeNil())
			Expect(*entry.Status.GUID).To(Equal(existingUUID))
		})

		It("adopts the existing entry when adopt is unset", func() {
			mockClient.EXPECT().Search(searchByDN).Return(found, nil)

			entry := newEntry(nil)
			Expect(reconciler.addEntry(mockClient, entry, server)).To(Succeed())
			Expect(entry.Status.GUID).NotTo(BeNil())
			Expect(*entry.Status.GUID).To(Equal(existingUUID))
		})

		It("does not adopt and surfaces the error when adopt is false", func() {
			mockClient.EXPECT().Search(searchByDN).Return(found, nil)

			entry := newEntry(boolptr.False())
			err := reconciler.addEntry(mockClient, entry, server)
			Expect(err).To(MatchError(ContainSubstring("already exists and adoption is disabled")))
			Expect(entry.Status.GUID).To(BeNil())
		})
	})

	Context("When updating an entry", func() {
		const (
			baseDN       = "dc=example,dc=org"
			existingDN   = "cn=existing,dc=example,dc=org"
			existingUUID = "3a5e9b2c-0000-4a1b-8c2d-1122334455ff"
		)

		var (
			mockClient *mock_ldap.MockClient
			reconciler *EntryReconciler
			server     *klapv1alpha1.Server
		)

		// newEntry builds an Entry already bound to existingUUID, targeting dn.
		newEntry := func(dn string, force *bool) *klapv1alpha1.Entry {
			guid := existingUUID
			return &klapv1alpha1.Entry{
				Spec: klapv1alpha1.EntrySpec{
					DN:    &dn,
					Force: force,
					Attributes: map[string][]string{
						objectClass: {organizationalUnit},
					},
				},
				Status: klapv1alpha1.EntryStatus{
					GUID: &guid,
				},
			}
		}

		// found returns the result of a GUID search locating the entry at dn.
		found := func(dn string, attrs ...*ldap.EntryAttribute) *ldap.SearchResult {
			return &ldap.SearchResult{
				Entries: []*ldap.Entry{
					{
						DN: dn,
						// The "*" selector returns user attributes only, so no GUID here.
						Attributes: append([]*ldap.EntryAttribute{
							{Name: objectClass, Values: []string{organizationalUnit}},
						}, attrs...),
					},
				},
			}
		}

		// searchByGUID matches the subtree search locating the entry by its GUID.
		searchByGUID := gomock.Cond(func(search *ldap.SearchRequest) bool {
			return search.BaseDN == baseDN && search.Scope == ldap.ScopeWholeSubtree
		})

		BeforeEach(func() {
			mockCtrl := gomock.NewController(GinkgoT())
			mockClient = mock_ldap.NewMockClient(mockCtrl)
			reconciler = &EntryReconciler{}

			base := baseDN
			impl := OpenLDAP
			server = &klapv1alpha1.Server{
				Spec: klapv1alpha1.ServerSpec{
					BaseDN:         &base,
					Implementation: &impl,
				},
			}
		})

		It("does not rename the entry when its DN is unchanged", func() {
			// No ModifyDN nor Modify is expected: gomock would fail on an unexpected call.
			mockClient.EXPECT().Search(searchByGUID).Return(found(existingDN), nil)

			Expect(reconciler.updateEntry(mockClient, newEntry(existingDN, nil), server)).To(Succeed())
		})

		It("does not rename the entry when its DN only differs by case", func() {
			mockClient.EXPECT().Search(searchByGUID).Return(found("CN=Existing, DC=example, DC=org"), nil)

			Expect(reconciler.updateEntry(mockClient, newEntry(existingDN, nil), server)).To(Succeed())
		})

		It("renames the entry when its DN has moved", func() {
			gomock.InOrder(
				mockClient.EXPECT().Search(searchByGUID).Return(found("cn=old,ou=people,dc=example,dc=org"), nil),
				mockClient.EXPECT().ModifyDN(gomock.Cond(func(moddn *ldap.ModifyDNRequest) bool {
					return moddn.DN == "cn=old,ou=people,dc=example,dc=org" &&
						moddn.NewRDN == "cn=existing" &&
						moddn.DeleteOldRDN &&
						moddn.NewSuperior == baseDN
				})).Return(nil),
			)

			Expect(reconciler.updateEntry(mockClient, newEntry(existingDN, nil), server)).To(Succeed())
		})

		It("surfaces a rename failure", func() {
			gomock.InOrder(
				mockClient.EXPECT().Search(searchByGUID).Return(found("cn=old,dc=example,dc=org"), nil),
				mockClient.EXPECT().ModifyDN(gomock.Any()).
					Return(ldap.NewError(ldap.LDAPResultEntryAlreadyExists, fmt.Errorf("already exists"))),
			)

			err := reconciler.updateEntry(mockClient, newEntry(existingDN, nil), server)
			Expect(ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists)).To(BeTrue())
		})

		It("keeps the RDN attributes when forcing, whatever their case", func() {
			dn := "CN=existing+UID=jdoe,dc=example,dc=org"
			gomock.InOrder(
				mockClient.EXPECT().Search(searchByGUID).Return(found(dn,
					&ldap.EntryAttribute{Name: "cn", Values: []string{"existing"}},
					&ldap.EntryAttribute{Name: "uid", Values: []string{"jdoe"}},
					&ldap.EntryAttribute{Name: description, Values: []string{"stale"}},
				), nil),
				mockClient.EXPECT().Modify(gomock.Cond(func(modify *ldap.ModifyRequest) bool {
					// Only the attribute absent from both the specification and the RDN is deleted.
					return len(modify.Changes) == 1 &&
						modify.Changes[0].Operation == ldap.DeleteAttribute &&
						modify.Changes[0].Modification.Type == description
				})).Return(nil),
			)

			Expect(reconciler.updateEntry(mockClient, newEntry(dn, boolptr.True()), server)).To(Succeed())
		})
	})
})
