package helm

import (
	"strings"
	"testing"

	"github.com/mgoltzsche/khelm/v2/internal/matcher"
	"github.com/mgoltzsche/khelm/v2/pkg/config"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

const (
	testForceNamespace      = "forced-ns"
	testOriginalSANamespace = "cluster-role-binding-ns"
	testOtherSubjectNS      = "other-ns"
	testRoleBindingName     = "my-role-binding"
)

func newTestTransformer(t *testing.T, forceNamespace string, forceSubjectNamespace bool, excludes ...config.ResourceSelector) manifestTransformer {
	t.Helper()
	return manifestTransformer{
		ForceNamespace:        forceNamespace,
		ForceSubjectNamespace: forceSubjectNamespace,
		Includes:              matcher.Any(),
		Excludes:              matcher.FromResourceSelectors(excludes),
	}
}

func transformString(t *testing.T, tr manifestTransformer, in string) []*yaml.RNode {
	t.Helper()
	out, err := tr.TransformManifest(strings.NewReader(in))
	require.NoError(t, err)
	return out
}

func marshalNodes(t *testing.T, nodes []*yaml.RNode) string {
	t.Helper()
	docs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		s, err := n.String()
		require.NoError(t, err)
		docs = append(docs, s)
	}
	return strings.Join(docs, "---\n")
}

// subjectInfo describes one entry of a binding's subjects list.
type subjectInfo struct {
	namespace        string
	namespacePresent bool
}

// subjectNamespaces maps subject name -> info for every entry of the subjects
// list of the given binding node.
func subjectNamespaces(t *testing.T, binding *yaml.RNode) map[string]subjectInfo {
	t.Helper()
	subjectsList, err := binding.Pipe(yaml.Lookup("subjects"))
	require.NoError(t, err)
	require.NotNil(t, subjectsList)
	subjects, err := subjectsList.Elements()
	require.NoError(t, err)
	infos := map[string]subjectInfo{}
	for _, s := range subjects {
		nameNode, err := s.Pipe(yaml.Lookup("name"))
		require.NoError(t, err)
		require.NotNil(t, nameNode)
		nsNode, err := s.Pipe(yaml.Lookup(yaml.NamespaceField))
		require.NoError(t, err)
		info := subjectInfo{namespacePresent: nsNode != nil}
		if nsNode != nil {
			info.namespace = nsNode.YNode().Value
		}
		infos[nameNode.YNode().Value] = info
	}
	return infos
}

const mixedSubjectsClusterRoleBinding = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ` + testRoleBindingName + `
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
- kind: ServiceAccount
  name: jenkins
  namespace: ` + testOriginalSANamespace + `
- kind: User
  name: jane@example.com
  namespace: ` + testOtherSubjectNS + `
- kind: Group
  name: developers
  namespace: ` + testOtherSubjectNS + `
`

// Only the ServiceAccount subject's namespace is rewritten; User and Group
// subjects are left untouched.
func TestTransformClusterRoleBindingRewritesOnlyServiceAccountSubjects(t *testing.T) {
	tr := newTestTransformer(t, testForceNamespace, false)
	out := transformString(t, tr, mixedSubjectsClusterRoleBinding)
	require.Len(t, out, 1)

	infos := subjectNamespaces(t, out[0])
	require.Equal(t, testForceNamespace, infos["jenkins"].namespace,
		"ServiceAccount subject namespace must be forced")
	require.True(t, infos["jenkins"].namespacePresent)
	require.Equal(t, testOtherSubjectNS, infos["jane@example.com"].namespace,
		"User subject namespace must not be rewritten")
	require.Equal(t, testOtherSubjectNS, infos["developers"].namespace,
		"Group subject namespace must not be rewritten")
}

// A ServiceAccount subject without namespace gets the forced namespace added,
// since Kubernetes requires the field for ServiceAccount subjects. User/Group
// subjects without namespace keep no field rather than getting one created.
func TestTransformClusterRoleBindingAddsMissingServiceAccountNamespace(t *testing.T) {
	in := `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ` + testRoleBindingName + `
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
- kind: ServiceAccount
  name: jenkins
- kind: User
  name: jane@example.com
- kind: Group
  name: developers
`
	tr := newTestTransformer(t, testForceNamespace, false)
	out := transformString(t, tr, in)
	require.Len(t, out, 1)

	infos := subjectNamespaces(t, out[0])
	sa := infos["jenkins"]
	require.True(t, sa.namespacePresent, "missing ServiceAccount subject namespace must be created")
	require.Equal(t, testForceNamespace, sa.namespace)
	require.False(t, infos["jane@example.com"].namespacePresent,
		"User subject must not get a namespace field created")
	require.False(t, infos["developers"].namespacePresent,
		"Group subject must not get a namespace field created")
}

// With no ForceNamespace the binding is returned byte-identical, including the
// original subject namespace (example/namespace relies on this behaviour).
func TestTransformWithoutForceNamespaceLeavesSubjectsUntouched(t *testing.T) {
	tr := newTestTransformer(t, "", false)
	out := transformString(t, tr, mixedSubjectsClusterRoleBinding)
	require.Len(t, out, 1)

	got, err := out[0].String()
	require.NoError(t, err)
	require.YAMLEq(t, mixedSubjectsClusterRoleBinding, got)

	infos := subjectNamespaces(t, out[0])
	for name, info := range infos {
		require.True(t, info.namespacePresent, "subject %s should keep its namespace", name)
		require.NotEqual(t, "", info.namespace)
	}
	require.Equal(t, testOriginalSANamespace, infos["jenkins"].namespace)
}

// RoleBinding subjects are never rewritten unless the opt-in switch is enabled.
func TestTransformRoleBindingSubjectsSwitch(t *testing.T) {
	in := `apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ` + testRoleBindingName + `
  namespace: source-ns
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: pod-reader
subjects:
- kind: ServiceAccount
  name: jenkins
  namespace: ` + testOriginalSANamespace + `
- kind: User
  name: jane@example.com
  namespace: ` + testOtherSubjectNS + `
`

	t.Run("default keeps subjects and forces resource namespace", func(t *testing.T) {
		tr := newTestTransformer(t, testForceNamespace, false)
		out := transformString(t, tr, in)
		require.Len(t, out, 1)

		meta, err := out[0].GetMeta()
		require.NoError(t, err)
		require.Equal(t, testForceNamespace, meta.Namespace,
			"the RoleBinding's own namespace is still forced")

		infos := subjectNamespaces(t, out[0])
		require.Equal(t, testOriginalSANamespace, infos["jenkins"].namespace,
			"RoleBinding ServiceAccount subject must be untouched by default")
		require.Equal(t, testOtherSubjectNS, infos["jane@example.com"].namespace)
	})

	t.Run("enabled rewrites ServiceAccount subject only", func(t *testing.T) {
		tr := newTestTransformer(t, testForceNamespace, true)
		out := transformString(t, tr, in)
		require.Len(t, out, 1)

		infos := subjectNamespaces(t, out[0])
		require.Equal(t, testForceNamespace, infos["jenkins"].namespace)
		require.Equal(t, testOtherSubjectNS, infos["jane@example.com"].namespace,
			"User subject must not be rewritten even with the switch enabled")
	})

	t.Run("enabled without force namespace is a no-op", func(t *testing.T) {
		tr := newTestTransformer(t, "", true)
		out := transformString(t, tr, in)
		require.Len(t, out, 1)
		infos := subjectNamespaces(t, out[0])
		require.Equal(t, testOriginalSANamespace, infos["jenkins"].namespace)
	})
}

// Excluded resources are dropped before any namespace rewriting happens, so an
// excluded binding never appears in the output at all.
func TestTransformExcludedBindingIsNotRewritten(t *testing.T) {
	tr := newTestTransformer(t, testForceNamespace, true, config.ResourceSelector{
		APIVersion: "rbac.authorization.k8s.io/v1",
		Kind:       "ClusterRoleBinding",
		Name:       testRoleBindingName,
	})
	out := transformString(t, tr, mixedSubjectsClusterRoleBinding)
	require.Empty(t, out, "excluded resource must not be emitted")
}

// Resources not matching the configured include selectors are dropped, hence
// never rewritten either.
func TestTransformNotIncludedBindingIsDropped(t *testing.T) {
	tr := manifestTransformer{
		ForceNamespace:        testForceNamespace,
		ForceSubjectNamespace: true,
		Includes: matcher.FromResourceSelectors([]config.ResourceSelector{{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		}}),
		Excludes: matcher.FromResourceSelectors(nil),
	}
	out := transformString(t, tr, mixedSubjectsClusterRoleBinding)
	require.Empty(t, out)
}

// ClusterRoleBindings nested in a List (even nested lists) are flattened and
// each subject rewritten exactly once.
func TestTransformListEmbeddedBindings(t *testing.T) {
	in := `apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: List
  items:
  - apiVersion: rbac.authorization.k8s.io/v1
    kind: ClusterRoleBinding
    metadata:
      name: nested-binding
    roleRef:
      apiGroup: rbac.authorization.k8s.io
      kind: ClusterRole
      name: cluster-admin
    subjects:
    - kind: ServiceAccount
      name: jenkins
      namespace: ` + testOriginalSANamespace + `
- apiVersion: rbac.authorization.k8s.io/v1
  kind: ClusterRoleBinding
  metadata:
    name: sibling-binding
  roleRef:
    apiGroup: rbac.authorization.k8s.io
    kind: ClusterRole
    name: cluster-admin
  subjects:
  - kind: ServiceAccount
    name: agent
    namespace: ` + testOriginalSANamespace + `
`
	tr := newTestTransformer(t, testForceNamespace, false)
	out := transformString(t, tr, in)
	require.Len(t, out, 2, "List items must be flattened into separate resources")

	byName := map[string]*yaml.RNode{}
	for _, n := range out {
		meta, err := n.GetMeta()
		require.NoError(t, err)
		byName[meta.Name] = n
	}
	require.Contains(t, byName, "nested-binding")
	require.Contains(t, byName, "sibling-binding")
	for name, node := range byName {
		infos := subjectNamespaces(t, node)
		for subject, info := range infos {
			require.Equal(t, testForceNamespace, info.namespace,
				"%s/%s subject must be rewritten once", name, subject)
		}
	}
}

// Applying the transformation twice yields exactly the same output as once.
func TestTransformIsIdempotent(t *testing.T) {
	tr := newTestTransformer(t, testForceNamespace, true)
	first := transformString(t, tr, mixedSubjectsClusterRoleBinding)
	firstSerialized := marshalNodes(t, first)

	second := transformString(t, tr, firstSerialized)
	secondSerialized := marshalNodes(t, second)
	require.Equal(t, firstSerialized, secondSerialized,
		"repeated transformation must not change the output")
}

// Resources of unknown kinds are not recognized by the openapi schema and
// therefore never get their subjects rewritten (their metadata.namespace may
// still be forced, which is pre-existing behaviour).
func TestTransformUnknownKindSubjectsUntouched(t *testing.T) {
	in := `apiVersion: example.com/v1
kind: TotallyUnknownBindingResource
metadata:
  name: custom-binding
subjects:
- kind: ServiceAccount
  name: jenkins
  namespace: ` + testOriginalSANamespace + `
`
	tr := newTestTransformer(t, testForceNamespace, true)
	out := transformString(t, tr, in)
	require.Len(t, out, 1)

	infos := subjectNamespaces(t, out[0])
	require.Equal(t, testOriginalSANamespace, infos["jenkins"].namespace,
		"unknown kind subjects must not be rewritten")
}

// The full transformed ClusterRoleBinding document matches the expected shape:
// the binding itself stays cluster-scoped (no metadata.namespace) while only
// the ServiceAccount subject namespace is rewritten.
func TestTransformForcedClusterRoleBindingDocument(t *testing.T) {
	tr := newTestTransformer(t, testForceNamespace, false)
	out := transformString(t, tr, mixedSubjectsClusterRoleBinding)
	require.Len(t, out, 1)

	meta, err := out[0].GetMeta()
	require.NoError(t, err)
	require.Empty(t, meta.Namespace, "cluster-scoped binding must not get a metadata.namespace")

	expected := `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ` + testRoleBindingName + `
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
- kind: ServiceAccount
  name: jenkins
  namespace: ` + testForceNamespace + `
- kind: User
  name: jane@example.com
  namespace: ` + testOtherSubjectNS + `
- kind: Group
  name: developers
  namespace: ` + testOtherSubjectNS + `
`
	got, err := out[0].String()
	require.NoError(t, err)
	require.YAMLEq(t, expected, got)
}

// A binding without subjects (or with an empty list) is handled without error.
func TestTransformBindingWithoutSubjects(t *testing.T) {
	in := `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ` + testRoleBindingName + `
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects: []
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: no-subjects-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
`
	tr := newTestTransformer(t, testForceNamespace, true)
	out := transformString(t, tr, in)
	require.Len(t, out, 2)
}

// NamespacedOnly and the subject rewriting are independent: a cluster-scoped
// binding still triggers the NamespacedOnly rejection even though its subjects
// would be rewritten, and without it the same input transforms successfully.
func TestTransformNamespacedOnlyStillRejectsClusterScopedBinding(t *testing.T) {
	tr := manifestTransformer{
		ForceNamespace:        testForceNamespace,
		ForceSubjectNamespace: true,
		Includes:              matcher.Any(),
		Excludes:              matcher.FromResourceSelectors(nil),
		NamespacedOnly:        true,
	}
	_, err := tr.TransformManifest(strings.NewReader(mixedSubjectsClusterRoleBinding))
	require.Error(t, err)
	require.Contains(t, err.Error(), "cluster-scoped")
}
