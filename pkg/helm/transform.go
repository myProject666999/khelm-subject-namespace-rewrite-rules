package helm

import (
	"fmt"
	"io"
	"strings"

	"github.com/mgoltzsche/khelm/v2/internal/matcher"
	"github.com/pkg/errors"
	"sigs.k8s.io/kustomize/kyaml/openapi"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

type manifestTransformer struct {
	ForceNamespace        string
	ForceSubjectNamespace bool
	Includes              matcher.ResourceMatchers
	Excludes              matcher.ResourceMatchers
	NamespacedOnly        bool
}

func (t *manifestTransformer) TransformManifest(manifest io.Reader) (r []*yaml.RNode, err error) {
	clusterScopedResources := []string{}
	d := yaml.NewDecoder(manifest)
	for {
		v := yaml.Node{}
		o := yaml.NewRNode(&v)
		err = d.Decode(&v)
		if err != nil {
			break
		}

		if o.IsNilOrEmpty() {
			continue
		}

		err = t.addResources(o, &r, &clusterScopedResources)
		if err != nil {
			break
		}
	}
	if err == io.EOF {
		err = nil
	} else if err != nil {
		return nil, errors.Wrap(err, "process helm output")
	}
	if len(clusterScopedResources) > 0 {
		return nil, errors.Errorf("manifests should only include namespace-scoped resources "+
			"but the following cluster-scoped (or unknown) resources have been found:\n * %s\nPlease exclude cluster-scoped resources or enable their usage", strings.Join(clusterScopedResources, "\n * "))
	}
	return
}

func (t *manifestTransformer) addResources(o *yaml.RNode, r *[]*yaml.RNode, clusterScopedResources *[]string) error {
	meta, err := o.GetMeta()
	if err != nil {
		return err
	}

	if meta.Kind == "List" && meta.APIVersion == "v1" { // Flatten list
		if m := o.Field("items"); m != nil {
			items, err := m.Value.Elements()
			if err != nil {
				return errors.Wrap(err, "get List resource items")
			}
			for _, item := range items {
				if err = t.addResources(item, r, clusterScopedResources); err != nil {
					return err
				}
			}
		}
		return nil
	}

	// Exclude all not explicitly included resources
	if !t.Includes.Match(&meta) {
		return nil
	}

	// Exclude resources
	if t.Excludes.Match(&meta) {
		return nil
	}

	// Set namespace
	err = t.applyNamespace(o, clusterScopedResources)
	if err != nil {
		return err
	}
	*r = append(*r, o)
	return nil
}

func (t *manifestTransformer) applyNamespace(o *yaml.RNode, clusterScopedResources *[]string) error {
	meta, err := o.GetMeta()
	if err != nil {
		return nil
	}
	namespaced, knownKind := openapi.IsNamespaceScoped(meta.TypeMeta)
	if t.ForceNamespace != "" && (namespaced || !knownKind) {
		// Forcefully set namespace on resource
		err = o.PipeE(
			yaml.LookupCreate(yaml.MappingNode, yaml.MetadataField),
			yaml.FieldClearer{Name: yaml.NamespaceField})
		if err != nil {
			return err
		}
		err = o.PipeE(
			yaml.LookupCreate(yaml.ScalarNode, yaml.MetadataField, yaml.NamespaceField),
			yaml.FieldSetter{StringValue: t.ForceNamespace})
		if err != nil {
			return err
		}
	} else if t.NamespacedOnly && (!namespaced || !knownKind) && meta.Namespace == "" {
		// Collect cluster-scoped resources to warn about them
		resID := fmt.Sprintf("apiVersion: %s, kind: %s, name: %s", meta.APIVersion, meta.Kind, meta.Name)
		*clusterScopedResources = append(*clusterScopedResources, resID)
	}
	// Set namespace of ServiceAccount references within role bindings
	if err = t.applySubjectNamespace(o, namespaced, knownKind); err != nil {
		return err
	}
	return nil
}

// applySubjectNamespace forces the namespace of ServiceAccount subjects onto
// ForceNamespace. Only subjects of kind ServiceAccount are touched: User and
// Group subjects are left unchanged. A missing namespace field on a
// ServiceAccount subject is created, since Kubernetes requires it.
//
// Cluster-scoped, known kinds (e.g. ClusterRoleBinding) are rewritten whenever
// ForceNamespace is set. Namespace-scoped bindings (e.g. RoleBinding) are only
// rewritten when ForceSubjectNamespace is enabled, to preserve cross-namespace
// references by default. Resources of unknown kinds are never touched.
func (t *manifestTransformer) applySubjectNamespace(o *yaml.RNode, namespaced, knownKind bool) error {
	if t.ForceNamespace == "" || !knownKind {
		return nil
	}
	if namespaced && !t.ForceSubjectNamespace {
		return nil
	}
	subjectsList, err := o.Pipe(yaml.Lookup("subjects"))
	if err != nil {
		return err
	}
	if subjectsList == nil {
		return nil
	}
	subjects, err := subjectsList.Elements()
	if err != nil {
		return err
	}
	for _, s := range subjects {
		kindNode, err := s.Pipe(yaml.Lookup(yaml.KindField))
		if err != nil {
			return err
		}
		if kindNode == nil || kindNode.YNode().Value != "ServiceAccount" {
			continue
		}
		if err = s.PipeE(
			yaml.LookupCreate(yaml.ScalarNode, yaml.NamespaceField),
			yaml.FieldSetter{StringValue: t.ForceNamespace}); err != nil {
			return err
		}
	}
	return nil
}
