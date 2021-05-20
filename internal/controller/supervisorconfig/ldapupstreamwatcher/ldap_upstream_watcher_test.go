// Copyright 2020-2021 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package ldapupstreamwatcher

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"

	"go.pinniped.dev/generated/latest/apis/supervisor/idp/v1alpha1"
	pinnipedfake "go.pinniped.dev/generated/latest/client/supervisor/clientset/versioned/fake"
	pinnipedinformers "go.pinniped.dev/generated/latest/client/supervisor/informers/externalversions"
	"go.pinniped.dev/internal/certauthority"
	"go.pinniped.dev/internal/controllerlib"
	"go.pinniped.dev/internal/mocks/mockldapconn"
	"go.pinniped.dev/internal/oidc/provider"
	"go.pinniped.dev/internal/testutil"
	"go.pinniped.dev/internal/upstreamldap"
)

func TestLDAPUpstreamWatcherControllerFilterSecrets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		secret     metav1.Object
		wantAdd    bool
		wantUpdate bool
		wantDelete bool
	}{
		{
			name: "a secret of the right type",
			secret: &corev1.Secret{
				Type:       corev1.SecretTypeBasicAuth,
				ObjectMeta: metav1.ObjectMeta{Name: "some-name", Namespace: "some-namespace"},
			},
			wantAdd:    true,
			wantUpdate: true,
			wantDelete: true,
		},
		{
			name: "a secret of the wrong type",
			secret: &corev1.Secret{
				Type:       "this-is-the-wrong-type",
				ObjectMeta: metav1.ObjectMeta{Name: "some-name", Namespace: "some-namespace"},
			},
		},
		{
			name: "resource of a data type which is not watched by this controller",
			secret: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: "some-name", Namespace: "some-namespace"},
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fakePinnipedClient := pinnipedfake.NewSimpleClientset()
			pinnipedInformers := pinnipedinformers.NewSharedInformerFactory(fakePinnipedClient, 0)
			ldapIDPInformer := pinnipedInformers.IDP().V1alpha1().LDAPIdentityProviders()
			fakeKubeClient := fake.NewSimpleClientset()
			kubeInformers := informers.NewSharedInformerFactory(fakeKubeClient, 0)
			secretInformer := kubeInformers.Core().V1().Secrets()
			withInformer := testutil.NewObservableWithInformerOption()

			New(nil, nil, ldapIDPInformer, secretInformer, withInformer.WithInformer)

			unrelated := corev1.Secret{}
			filter := withInformer.GetFilterForInformer(secretInformer)
			require.Equal(t, test.wantAdd, filter.Add(test.secret))
			require.Equal(t, test.wantUpdate, filter.Update(&unrelated, test.secret))
			require.Equal(t, test.wantUpdate, filter.Update(test.secret, &unrelated))
			require.Equal(t, test.wantDelete, filter.Delete(test.secret))
		})
	}
}

func TestLDAPUpstreamWatcherControllerFilterLDAPIdentityProviders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		idp        metav1.Object
		wantAdd    bool
		wantUpdate bool
		wantDelete bool
	}{
		{
			name: "any LDAPIdentityProvider",
			idp: &v1alpha1.LDAPIdentityProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "some-name", Namespace: "some-namespace"},
			},
			wantAdd:    true,
			wantUpdate: true,
			wantDelete: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fakePinnipedClient := pinnipedfake.NewSimpleClientset()
			pinnipedInformers := pinnipedinformers.NewSharedInformerFactory(fakePinnipedClient, 0)
			ldapIDPInformer := pinnipedInformers.IDP().V1alpha1().LDAPIdentityProviders()
			fakeKubeClient := fake.NewSimpleClientset()
			kubeInformers := informers.NewSharedInformerFactory(fakeKubeClient, 0)
			secretInformer := kubeInformers.Core().V1().Secrets()
			withInformer := testutil.NewObservableWithInformerOption()

			New(nil, nil, ldapIDPInformer, secretInformer, withInformer.WithInformer)

			unrelated := corev1.Secret{}
			filter := withInformer.GetFilterForInformer(ldapIDPInformer)
			require.Equal(t, test.wantAdd, filter.Add(test.idp))
			require.Equal(t, test.wantUpdate, filter.Update(&unrelated, test.idp))
			require.Equal(t, test.wantUpdate, filter.Update(test.idp, &unrelated))
			require.Equal(t, test.wantDelete, filter.Delete(test.idp))
		})
	}
}

// Wrap the func into a struct so the test can do deep equal assertions on instances of upstreamldap.Provider.
type comparableDialer struct {
	upstreamldap.LDAPDialerFunc
}

func TestLDAPUpstreamWatcherControllerSync(t *testing.T) {
	t.Parallel()
	now := metav1.NewTime(time.Now().UTC())

	const (
		testNamespace         = "test-namespace"
		testName              = "test-name"
		testSecretName        = "test-bind-secret"
		testBindUsername      = "test-bind-username"
		testBindPassword      = "test-bind-password"
		testHost              = "ldap.example.com:123"
		testUserSearchBase    = "test-user-search-base"
		testUserSearchFilter  = "test-user-search-filter"
		testGroupSearchBase   = "test-group-search-base"
		testGroupSearchFilter = "test-group-search-filter"
		testUsernameAttrName  = "test-username-attr"
		testGroupNameAttrName = "test-group-name-attr"
		testUIDAttrName       = "test-uid-attr"
	)

	testValidSecretData := map[string][]byte{"username": []byte(testBindUsername), "password": []byte(testBindPassword)}

	testCA, err := certauthority.New("test CA", time.Minute)
	require.NoError(t, err)
	testCABundle := testCA.Bundle()
	testCABundleBase64Encoded := base64.StdEncoding.EncodeToString(testCABundle)

	validUpstream := &v1alpha1.LDAPIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace, Generation: 1234},
		Spec: v1alpha1.LDAPIdentityProviderSpec{
			Host: testHost,
			TLS:  &v1alpha1.TLSSpec{CertificateAuthorityData: testCABundleBase64Encoded},
			Bind: v1alpha1.LDAPIdentityProviderBind{SecretName: testSecretName},
			UserSearch: v1alpha1.LDAPIdentityProviderUserSearch{
				Base:   testUserSearchBase,
				Filter: testUserSearchFilter,
				Attributes: v1alpha1.LDAPIdentityProviderUserSearchAttributes{
					Username: testUsernameAttrName,
					UID:      testUIDAttrName,
				},
			},
			GroupSearch: v1alpha1.LDAPIdentityProviderGroupSearch{
				Base:   testGroupSearchBase,
				Filter: testGroupSearchFilter,
				Attributes: v1alpha1.LDAPIdentityProviderGroupSearchAttributes{
					GroupName: testGroupNameAttrName,
				},
			},
		},
	}
	editedValidUpstream := func(editFunc func(*v1alpha1.LDAPIdentityProvider)) *v1alpha1.LDAPIdentityProvider {
		deepCopy := validUpstream.DeepCopy()
		editFunc(deepCopy)
		return deepCopy
	}

	providerConfigForValidUpstream := &upstreamldap.ProviderConfig{
		Name:         testName,
		Host:         testHost,
		CABundle:     testCABundle,
		BindUsername: testBindUsername,
		BindPassword: testBindPassword,
		UserSearch: upstreamldap.UserSearchConfig{
			Base:              testUserSearchBase,
			Filter:            testUserSearchFilter,
			UsernameAttribute: testUsernameAttrName,
			UIDAttribute:      testUIDAttrName,
		},
		GroupSearch: upstreamldap.GroupSearchConfig{
			Base:               testGroupSearchBase,
			Filter:             testGroupSearchFilter,
			GroupNameAttribute: testGroupNameAttrName,
		},
	}

	bindSecretValidTrueCondition := func(gen int64) v1alpha1.Condition {
		return v1alpha1.Condition{
			Type:               "BindSecretValid",
			Status:             "True",
			LastTransitionTime: now,
			Reason:             "Success",
			Message:            "loaded bind secret",
			ObservedGeneration: gen,
		}
	}
	ldapConnectionValidTrueCondition := func(gen int64, secretVersion string) v1alpha1.Condition {
		return v1alpha1.Condition{
			Type:               "LDAPConnectionValid",
			Status:             "True",
			LastTransitionTime: now,
			Reason:             "Success",
			Message: fmt.Sprintf(
				`successfully able to connect to "%s" and bind as user "%s" [validated with Secret "%s" at version "%s"]`,
				testHost, testBindUsername, testSecretName, secretVersion),
			ObservedGeneration: gen,
		}
	}
	tlsConfigurationValidLoadedTrueCondition := func(gen int64) v1alpha1.Condition {
		return v1alpha1.Condition{
			Type:               "TLSConfigurationValid",
			Status:             "True",
			LastTransitionTime: now,
			Reason:             "Success",
			Message:            "loaded TLS configuration",
			ObservedGeneration: gen,
		}
	}
	allConditionsTrue := func(gen int64, secretVersion string) []v1alpha1.Condition {
		return []v1alpha1.Condition{
			bindSecretValidTrueCondition(gen),
			ldapConnectionValidTrueCondition(gen, secretVersion),
			tlsConfigurationValidLoadedTrueCondition(gen),
		}
	}

	validBindUserSecret := func(secretVersion string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testSecretName, Namespace: testNamespace, ResourceVersion: secretVersion},
			Type:       corev1.SecretTypeBasicAuth,
			Data:       testValidSecretData,
		}
	}

	tests := []struct {
		name                           string
		initialValidatedSecretVersions map[string]string
		inputUpstreams                 []runtime.Object
		inputSecrets                   []runtime.Object
		setupMocks                     func(conn *mockldapconn.MockConn)
		dialError                      error
		wantErr                        string
		wantResultingCache             []*upstreamldap.ProviderConfig
		wantResultingUpstreams         []v1alpha1.LDAPIdentityProvider
		wantValidatedSecretVersions    map[string]string
	}{
		{
			name:               "no LDAPIdentityProvider upstreams clears the cache",
			wantResultingCache: []*upstreamldap.ProviderConfig{},
		},
		{
			name:           "one valid upstream updates the cache to include only that upstream",
			inputUpstreams: []runtime.Object{validUpstream},
			inputSecrets:   []runtime.Object{validBindUserSecret("4242")},
			setupMocks: func(conn *mockldapconn.MockConn) {
				// Should perform a test dial and bind.
				conn.EXPECT().Bind(testBindUsername, testBindPassword).Times(1)
				conn.EXPECT().Close().Times(1)
			},
			wantResultingCache: []*upstreamldap.ProviderConfig{providerConfigForValidUpstream},
			wantResultingUpstreams: []v1alpha1.LDAPIdentityProvider{{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, Generation: 1234},
				Status: v1alpha1.LDAPIdentityProviderStatus{
					Phase:      "Ready",
					Conditions: allConditionsTrue(1234, "4242"),
				},
			}},
			wantValidatedSecretVersions: map[string]string{testName: "4242"},
		},
		{
			name:               "missing secret",
			inputUpstreams:     []runtime.Object{validUpstream},
			inputSecrets:       []runtime.Object{},
			wantErr:            controllerlib.ErrSyntheticRequeue.Error(),
			wantResultingCache: []*upstreamldap.ProviderConfig{},
			wantResultingUpstreams: []v1alpha1.LDAPIdentityProvider{{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, Generation: 1234},
				Status: v1alpha1.LDAPIdentityProviderStatus{
					Phase: "Error",
					Conditions: []v1alpha1.Condition{
						{
							Type:               "BindSecretValid",
							Status:             "False",
							LastTransitionTime: now,
							Reason:             "SecretNotFound",
							Message:            fmt.Sprintf(`secret "%s" not found`, testSecretName),
							ObservedGeneration: 1234,
						},
						tlsConfigurationValidLoadedTrueCondition(1234),
					},
				},
			}},
		},
		{
			name:           "secret has wrong type",
			inputUpstreams: []runtime.Object{validUpstream},
			inputSecrets: []runtime.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: testSecretName, Namespace: testNamespace},
				Type:       "some-other-type",
				Data:       testValidSecretData,
			}},
			wantErr:            controllerlib.ErrSyntheticRequeue.Error(),
			wantResultingCache: []*upstreamldap.ProviderConfig{},
			wantResultingUpstreams: []v1alpha1.LDAPIdentityProvider{{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, Generation: 1234},
				Status: v1alpha1.LDAPIdentityProviderStatus{
					Phase: "Error",
					Conditions: []v1alpha1.Condition{
						{
							Type:               "BindSecretValid",
							Status:             "False",
							LastTransitionTime: now,
							Reason:             "SecretWrongType",
							Message:            fmt.Sprintf(`referenced Secret "%s" has wrong type "some-other-type" (should be "kubernetes.io/basic-auth")`, testSecretName),
							ObservedGeneration: 1234,
						},
						tlsConfigurationValidLoadedTrueCondition(1234),
					},
				},
			}},
		},
		{
			name:           "secret is missing key",
			inputUpstreams: []runtime.Object{validUpstream},
			inputSecrets: []runtime.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: testSecretName, Namespace: testNamespace},
				Type:       corev1.SecretTypeBasicAuth,
			}},
			wantErr:            controllerlib.ErrSyntheticRequeue.Error(),
			wantResultingCache: []*upstreamldap.ProviderConfig{},
			wantResultingUpstreams: []v1alpha1.LDAPIdentityProvider{{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, Generation: 1234},
				Status: v1alpha1.LDAPIdentityProviderStatus{
					Phase: "Error",
					Conditions: []v1alpha1.Condition{
						{
							Type:               "BindSecretValid",
							Status:             "False",
							LastTransitionTime: now,
							Reason:             "SecretMissingKeys",
							Message:            fmt.Sprintf(`referenced Secret "%s" is missing required keys ["username" "password"]`, testSecretName),
							ObservedGeneration: 1234,
						},
						tlsConfigurationValidLoadedTrueCondition(1234),
					},
				},
			}},
		},
		{
			name: "CertificateAuthorityData is not base64 encoded",
			inputUpstreams: []runtime.Object{editedValidUpstream(func(upstream *v1alpha1.LDAPIdentityProvider) {
				upstream.Spec.TLS.CertificateAuthorityData = "this-is-not-base64-encoded"
			})},
			inputSecrets:       []runtime.Object{validBindUserSecret("")},
			wantErr:            controllerlib.ErrSyntheticRequeue.Error(),
			wantResultingCache: []*upstreamldap.ProviderConfig{},
			wantResultingUpstreams: []v1alpha1.LDAPIdentityProvider{{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, Generation: 1234},
				Status: v1alpha1.LDAPIdentityProviderStatus{
					Phase: "Error",
					Conditions: []v1alpha1.Condition{
						bindSecretValidTrueCondition(1234),
						{
							Type:               "TLSConfigurationValid",
							Status:             "False",
							LastTransitionTime: now,
							Reason:             "InvalidTLSConfig",
							Message:            "certificateAuthorityData is invalid: illegal base64 data at input byte 4",
							ObservedGeneration: 1234,
						},
					},
				},
			}},
		},
		{
			name: "CertificateAuthorityData is not valid pem data",
			inputUpstreams: []runtime.Object{editedValidUpstream(func(upstream *v1alpha1.LDAPIdentityProvider) {
				upstream.Spec.TLS.CertificateAuthorityData = base64.StdEncoding.EncodeToString([]byte("this is not pem data"))
			})},
			inputSecrets:       []runtime.Object{validBindUserSecret("")},
			wantErr:            controllerlib.ErrSyntheticRequeue.Error(),
			wantResultingCache: []*upstreamldap.ProviderConfig{},
			wantResultingUpstreams: []v1alpha1.LDAPIdentityProvider{{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, Generation: 1234},
				Status: v1alpha1.LDAPIdentityProviderStatus{
					Phase: "Error",
					Conditions: []v1alpha1.Condition{
						bindSecretValidTrueCondition(1234),
						{
							Type:               "TLSConfigurationValid",
							Status:             "False",
							LastTransitionTime: now,
							Reason:             "InvalidTLSConfig",
							Message:            "certificateAuthorityData is invalid: no certificates found",
							ObservedGeneration: 1234,
						},
					},
				},
			}},
		},
		{
			name: "nil TLS configuration is valid",
			inputUpstreams: []runtime.Object{editedValidUpstream(func(upstream *v1alpha1.LDAPIdentityProvider) {
				upstream.Spec.TLS = nil
			})},
			inputSecrets: []runtime.Object{validBindUserSecret("4242")},
			setupMocks: func(conn *mockldapconn.MockConn) {
				// Should perform a test dial and bind.
				conn.EXPECT().Bind(testBindUsername, testBindPassword).Times(1)
				conn.EXPECT().Close().Times(1)
			},
			wantResultingCache: []*upstreamldap.ProviderConfig{
				{
					Name:         testName,
					Host:         testHost,
					CABundle:     nil,
					BindUsername: testBindUsername,
					BindPassword: testBindPassword,
					UserSearch: upstreamldap.UserSearchConfig{
						Base:              testUserSearchBase,
						Filter:            testUserSearchFilter,
						UsernameAttribute: testUsernameAttrName,
						UIDAttribute:      testUIDAttrName,
					},
					GroupSearch: upstreamldap.GroupSearchConfig{
						Base:               testGroupSearchBase,
						Filter:             testGroupSearchFilter,
						GroupNameAttribute: testGroupNameAttrName,
					},
				},
			},
			wantResultingUpstreams: []v1alpha1.LDAPIdentityProvider{{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, Generation: 1234},
				Status: v1alpha1.LDAPIdentityProviderStatus{
					Phase: "Ready",
					Conditions: []v1alpha1.Condition{
						bindSecretValidTrueCondition(1234),
						ldapConnectionValidTrueCondition(1234, "4242"),
						{
							Type:               "TLSConfigurationValid",
							Status:             "True",
							LastTransitionTime: now,
							Reason:             "Success",
							Message:            "no TLS configuration provided",
							ObservedGeneration: 1234,
						},
					},
				},
			}},
			wantValidatedSecretVersions: map[string]string{testName: "4242"},
		},
		{
			name: "non-nil TLS configuration with empty CertificateAuthorityData is valid",
			inputUpstreams: []runtime.Object{editedValidUpstream(func(upstream *v1alpha1.LDAPIdentityProvider) {
				upstream.Spec.TLS.CertificateAuthorityData = ""
			})},
			inputSecrets: []runtime.Object{validBindUserSecret("4242")},
			setupMocks: func(conn *mockldapconn.MockConn) {
				// Should perform a test dial and bind.
				conn.EXPECT().Bind(testBindUsername, testBindPassword).Times(1)
				conn.EXPECT().Close().Times(1)
			},
			wantResultingCache: []*upstreamldap.ProviderConfig{
				{
					Name:         testName,
					Host:         testHost,
					CABundle:     nil,
					BindUsername: testBindUsername,
					BindPassword: testBindPassword,
					UserSearch: upstreamldap.UserSearchConfig{
						Base:              testUserSearchBase,
						Filter:            testUserSearchFilter,
						UsernameAttribute: testUsernameAttrName,
						UIDAttribute:      testUIDAttrName,
					},
					GroupSearch: upstreamldap.GroupSearchConfig{
						Base:               testGroupSearchBase,
						Filter:             testGroupSearchFilter,
						GroupNameAttribute: testGroupNameAttrName,
					},
				},
			},
			wantResultingUpstreams: []v1alpha1.LDAPIdentityProvider{{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, Generation: 1234},
				Status: v1alpha1.LDAPIdentityProviderStatus{
					Phase:      "Ready",
					Conditions: allConditionsTrue(1234, "4242"),
				},
			}},
			wantValidatedSecretVersions: map[string]string{testName: "4242"},
		},
		{
			name: "one valid upstream and one invalid upstream updates the cache to include only the valid upstream",
			inputUpstreams: []runtime.Object{validUpstream, editedValidUpstream(func(upstream *v1alpha1.LDAPIdentityProvider) {
				upstream.Name = "other-upstream"
				upstream.Generation = 42
				upstream.Spec.Bind.SecretName = "non-existent-secret"
			})},
			inputSecrets: []runtime.Object{validBindUserSecret("4242")},
			setupMocks: func(conn *mockldapconn.MockConn) {
				// Should perform a test dial and bind for the one valid upstream configuration.
				conn.EXPECT().Bind(testBindUsername, testBindPassword).Times(1)
				conn.EXPECT().Close().Times(1)
			},
			wantErr:            controllerlib.ErrSyntheticRequeue.Error(),
			wantResultingCache: []*upstreamldap.ProviderConfig{providerConfigForValidUpstream},
			wantResultingUpstreams: []v1alpha1.LDAPIdentityProvider{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "other-upstream", Generation: 42},
					Status: v1alpha1.LDAPIdentityProviderStatus{
						Phase: "Error",
						Conditions: []v1alpha1.Condition{
							{
								Type:               "BindSecretValid",
								Status:             "False",
								LastTransitionTime: now,
								Reason:             "SecretNotFound",
								Message:            fmt.Sprintf(`secret "%s" not found`, "non-existent-secret"),
								ObservedGeneration: 42,
							},
							tlsConfigurationValidLoadedTrueCondition(42),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, Generation: 1234},
					Status: v1alpha1.LDAPIdentityProviderStatus{
						Phase:      "Ready",
						Conditions: allConditionsTrue(1234, "4242"),
					},
				},
			},
			wantValidatedSecretVersions: map[string]string{testName: "4242"},
		},
		{
			name:           "when testing the connection to the LDAP server fails then the upstream is still added to the cache anyway (treated like a warning)",
			inputUpstreams: []runtime.Object{validUpstream},
			inputSecrets:   []runtime.Object{validBindUserSecret("")},
			setupMocks: func(conn *mockldapconn.MockConn) {
				// Should perform a test dial and bind.
				conn.EXPECT().Bind(testBindUsername, testBindPassword).Times(1).Return(errors.New("some bind error"))
				conn.EXPECT().Close().Times(1)
			},
			wantErr:            controllerlib.ErrSyntheticRequeue.Error(),
			wantResultingCache: []*upstreamldap.ProviderConfig{providerConfigForValidUpstream},
			wantResultingUpstreams: []v1alpha1.LDAPIdentityProvider{{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, Generation: 1234},
				Status: v1alpha1.LDAPIdentityProviderStatus{
					Phase: "Error",
					Conditions: []v1alpha1.Condition{
						bindSecretValidTrueCondition(1234),
						{
							Type:               "LDAPConnectionValid",
							Status:             "False",
							LastTransitionTime: now,
							Reason:             "LDAPConnectionError",
							Message: fmt.Sprintf(
								`could not successfully connect to "%s" and bind as user "%s": error binding as "%s": some bind error`,
								testHost, testBindUsername, testBindUsername),
							ObservedGeneration: 1234,
						},
						tlsConfigurationValidLoadedTrueCondition(1234),
					},
				},
			}},
		},
		{
			name: "when the LDAP server connection was already validated for the current resource generation and secret version, then do not validate it again",
			inputUpstreams: []runtime.Object{editedValidUpstream(func(upstream *v1alpha1.LDAPIdentityProvider) {
				upstream.Generation = 1234
				upstream.Status.Conditions = []v1alpha1.Condition{
					ldapConnectionValidTrueCondition(1234, "4242"),
				}
			})},
			inputSecrets:                   []runtime.Object{validBindUserSecret("4242")},
			initialValidatedSecretVersions: map[string]string{testName: "4242"},
			setupMocks: func(conn *mockldapconn.MockConn) {
				// Should not perform a test dial and bind. No mocking here means the test will fail if Bind() or Close() are called.
			},
			wantResultingCache: []*upstreamldap.ProviderConfig{providerConfigForValidUpstream},
			wantResultingUpstreams: []v1alpha1.LDAPIdentityProvider{{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, Generation: 1234},
				Status: v1alpha1.LDAPIdentityProviderStatus{
					Phase:      "Ready",
					Conditions: allConditionsTrue(1234, "4242"),
				},
			}},
			wantValidatedSecretVersions: map[string]string{testName: "4242"},
		},
		{
			name: "when the LDAP server connection was validated for an older resource generation, then try to validate it again",
			inputUpstreams: []runtime.Object{editedValidUpstream(func(upstream *v1alpha1.LDAPIdentityProvider) {
				upstream.Generation = 1234 // current generation
				upstream.Status.Conditions = []v1alpha1.Condition{
					ldapConnectionValidTrueCondition(1233, "4242"), // older spec generation!
				}
			})},
			inputSecrets:                   []runtime.Object{validBindUserSecret("4242")},
			initialValidatedSecretVersions: map[string]string{testName: "4242"},
			setupMocks: func(conn *mockldapconn.MockConn) {
				// Should perform a test dial and bind.
				conn.EXPECT().Bind(testBindUsername, testBindPassword).Times(1)
				conn.EXPECT().Close().Times(1)
			},
			wantResultingCache: []*upstreamldap.ProviderConfig{providerConfigForValidUpstream},
			wantResultingUpstreams: []v1alpha1.LDAPIdentityProvider{{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, Generation: 1234},
				Status: v1alpha1.LDAPIdentityProviderStatus{
					Phase:      "Ready",
					Conditions: allConditionsTrue(1234, "4242"),
				},
			}},
			wantValidatedSecretVersions: map[string]string{testName: "4242"},
		},
		{
			name: "when the LDAP server connection validation previously failed for this resource generation, then try to validate it again",
			inputUpstreams: []runtime.Object{editedValidUpstream(func(upstream *v1alpha1.LDAPIdentityProvider) {
				upstream.Generation = 1234
				upstream.Status.Conditions = []v1alpha1.Condition{
					{
						Type:               "LDAPConnectionValid",
						Status:             "False", // failure!
						LastTransitionTime: now,
						Reason:             "LDAPConnectionError",
						Message:            "some-error-message",
						ObservedGeneration: 1234, // same (current) generation!
					},
				}
			})},
			inputSecrets:                   []runtime.Object{validBindUserSecret("4242")},
			initialValidatedSecretVersions: map[string]string{testName: "1"},
			setupMocks: func(conn *mockldapconn.MockConn) {
				// Should perform a test dial and bind.
				conn.EXPECT().Bind(testBindUsername, testBindPassword).Times(1)
				conn.EXPECT().Close().Times(1)
			},
			wantResultingCache: []*upstreamldap.ProviderConfig{providerConfigForValidUpstream},
			wantResultingUpstreams: []v1alpha1.LDAPIdentityProvider{{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, Generation: 1234},
				Status: v1alpha1.LDAPIdentityProviderStatus{
					Phase:      "Ready",
					Conditions: allConditionsTrue(1234, "4242"),
				},
			}},
			wantValidatedSecretVersions: map[string]string{testName: "4242"},
		},
		{
			name: "when the LDAP server connection was already validated for this resource generation but the bind secret has changed, then try to validate it again",
			inputUpstreams: []runtime.Object{editedValidUpstream(func(upstream *v1alpha1.LDAPIdentityProvider) {
				upstream.Generation = 1234
				upstream.Status.Conditions = []v1alpha1.Condition{
					ldapConnectionValidTrueCondition(1234, "4241"), // same spec generation, old secret version
				}
			})},
			inputSecrets:                   []runtime.Object{validBindUserSecret("4242")}, // newer secret version!
			initialValidatedSecretVersions: map[string]string{testName: "4241"},           // old version was validated
			setupMocks: func(conn *mockldapconn.MockConn) {
				// Should perform a test dial and bind.
				conn.EXPECT().Bind(testBindUsername, testBindPassword).Times(1)
				conn.EXPECT().Close().Times(1)
			},
			wantResultingCache: []*upstreamldap.ProviderConfig{providerConfigForValidUpstream},
			wantResultingUpstreams: []v1alpha1.LDAPIdentityProvider{{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName, Generation: 1234},
				Status: v1alpha1.LDAPIdentityProviderStatus{
					Phase:      "Ready",
					Conditions: allConditionsTrue(1234, "4242"),
				},
			}},
			wantValidatedSecretVersions: map[string]string{testName: "4242"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fakePinnipedClient := pinnipedfake.NewSimpleClientset(tt.inputUpstreams...)
			pinnipedInformers := pinnipedinformers.NewSharedInformerFactory(fakePinnipedClient, 0)
			fakeKubeClient := fake.NewSimpleClientset(tt.inputSecrets...)
			kubeInformers := informers.NewSharedInformerFactory(fakeKubeClient, 0)
			cache := provider.NewDynamicUpstreamIDPProvider()
			cache.SetLDAPIdentityProviders([]provider.UpstreamLDAPIdentityProviderI{
				upstreamldap.New(upstreamldap.ProviderConfig{Name: "initial-entry"}),
			})

			ctrl := gomock.NewController(t)
			t.Cleanup(ctrl.Finish)

			conn := mockldapconn.NewMockConn(ctrl)
			if tt.setupMocks != nil {
				tt.setupMocks(conn)
			}

			dialer := &comparableDialer{upstreamldap.LDAPDialerFunc(func(ctx context.Context, _ string) (upstreamldap.Conn, error) {
				if tt.dialError != nil {
					return nil, tt.dialError
				}
				return conn, nil
			})}

			validatedSecretVersionCache := newSecretVersionCache()
			if tt.initialValidatedSecretVersions != nil {
				validatedSecretVersionCache.ResourceVersionsByName = tt.initialValidatedSecretVersions
			}

			controller := newInternal(
				cache,
				validatedSecretVersionCache,
				dialer,
				fakePinnipedClient,
				pinnipedInformers.IDP().V1alpha1().LDAPIdentityProviders(),
				kubeInformers.Core().V1().Secrets(),
				controllerlib.WithInformer,
			)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			pinnipedInformers.Start(ctx.Done())
			kubeInformers.Start(ctx.Done())
			controllerlib.TestRunSynchronously(t, controller)

			syncCtx := controllerlib.Context{Context: ctx, Key: controllerlib.Key{}}

			if err := controllerlib.TestSync(t, controller, syncCtx); tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}

			actualIDPList := cache.GetLDAPIdentityProviders()
			require.Equal(t, len(tt.wantResultingCache), len(actualIDPList))
			for i := range actualIDPList {
				actualIDP := actualIDPList[i].(*upstreamldap.Provider)
				copyOfExpectedValue := *tt.wantResultingCache[i] // copy before edit to avoid race because these tests are run in parallel
				// The dialer that was passed in to the controller's constructor should always have been
				// passed through to the provider.
				copyOfExpectedValue.Dialer = dialer
				require.Equal(t, copyOfExpectedValue, actualIDP.GetConfig())
			}

			actualUpstreams, err := fakePinnipedClient.IDPV1alpha1().LDAPIdentityProviders(testNamespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)

			// Assert on the expected Status of the upstreams. Preprocess the upstreams a bit so that they're easier to assert against.
			normalizedActualUpstreams := normalizeLDAPUpstreams(actualUpstreams.Items, now)
			require.Equal(t, len(tt.wantResultingUpstreams), len(normalizedActualUpstreams))
			for i := range tt.wantResultingUpstreams {
				// Require each separately to get a nice diff when the test fails.
				require.Equal(t, tt.wantResultingUpstreams[i], normalizedActualUpstreams[i])
			}

			// Check that the controller remembered which version of the secret it most recently validated successfully with.
			if tt.wantValidatedSecretVersions == nil {
				tt.wantValidatedSecretVersions = map[string]string{}
			}
			require.Equal(t, tt.wantValidatedSecretVersions, validatedSecretVersionCache.ResourceVersionsByName)
		})
	}
}

func normalizeLDAPUpstreams(upstreams []v1alpha1.LDAPIdentityProvider, now metav1.Time) []v1alpha1.LDAPIdentityProvider {
	result := make([]v1alpha1.LDAPIdentityProvider, 0, len(upstreams))
	for _, u := range upstreams {
		normalized := u.DeepCopy()

		// We're only interested in comparing the status, so zero out the spec.
		normalized.Spec = v1alpha1.LDAPIdentityProviderSpec{}

		// Round down the LastTransitionTime values to `now` if they were just updated. This makes
		// it much easier to encode assertions about the expected timestamps.
		for i := range normalized.Status.Conditions {
			if time.Since(normalized.Status.Conditions[i].LastTransitionTime.Time) < 5*time.Second {
				normalized.Status.Conditions[i].LastTransitionTime = now
			}
		}
		result = append(result, *normalized)
	}

	sort.SliceStable(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result
}