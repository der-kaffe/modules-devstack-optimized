package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/notify"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
)

// Metadata definitions with the original's default policies: any project
// reads public namespaces and its own private ones (admin reads all); only
// admin writes (metadef_admin). A protected namespace and its children cannot
// be deleted.

// NamespaceDetail is a namespace with what GET /namespaces/{ns} embeds.
type NamespaceDetail struct {
	model.Namespace
	Associations []model.ResourceTypeAssociation
	Properties   []model.MetadefProperty
	Objects      []model.MetadefObject
	Tags         []model.MetadefTag
}

func (s *Service) metadefs() (repository.Metadefs, error) {
	m, ok := s.repo.(repository.Metadefs)
	if !ok {
		return nil, errors.New("metadata definitions are not available")
	}
	return m, nil
}

func nsNotFound(name string) error {
	return model.Fail(model.NotFound, "Metadata definition namespace="+name+" was not found.")
}

func (s *Service) readNamespace(ctx context.Context, who auth.Identity, name string) (repository.Metadefs, model.Namespace, error) {
	return s.readNamespaceAs(ctx, who, name, false)
}

// readNamespaceAs: a namespace the caller cannot see answers as the
// original: the generic 404 for the namespace itself, "Namespace X not
// found" for its children.
func (s *Service) readNamespaceAs(ctx context.Context, who auth.Identity, name string, child bool) (repository.Metadefs, model.Namespace, error) {
	if err := who.Check(s.now()); err != nil {
		return nil, model.Namespace{}, err
	}
	repo, err := s.metadefs()
	if err != nil {
		return nil, model.Namespace{}, err
	}
	n, err := repo.GetNamespace(ctx, name)
	if errors.Is(err, repository.ErrMetadefNotFound) {
		return nil, n, nsNotFound(name)
	}
	if err != nil {
		return nil, n, err
	}
	if overridden() {
		if Permit(who, "get_metadef_namespace", namespaceTarget(n), nil) != nil {
			return nil, n, model.Fail(model.NotFound, "The resource could not be found.")
		}
		return repo, n, nil
	}
	if !who.HasRole("admin") && n.Visibility != "public" && n.Owner != who.ProjectID {
		if child {
			return nil, n, model.Fail(model.NotFound, "Namespace "+name+" not found")
		}
		return nil, n, model.Fail(model.NotFound, "The resource could not be found.")
	}
	return repo, n, nil
}

// writeNamespace: a namespace the caller can see, and admin (action names
// the original's policy rule in the 403 message).
func (s *Service) writeNamespace(ctx context.Context, who auth.Identity, name, action string) (repository.Metadefs, model.Namespace, error) {
	repo, n, err := s.readNamespaceAs(ctx, who, name, action != "modify_metadef_namespace" && action != "delete_metadef_namespace")
	if err != nil {
		return nil, n, err
	}
	if overridden() {
		if err = Permit(who, action, namespaceTarget(n), nil); err != nil {
			return nil, n, err
		}
	} else if err = metadefAdmin(who, action); err != nil {
		return nil, n, err
	}
	return repo, n, nil
}

func namespaceTarget(n model.Namespace) map[string]any {
	return map[string]any{"project_id": n.Owner, "owner": n.Owner, "visibility": n.Visibility, "namespace": n.Namespace}
}

func metadefAdmin(who auth.Identity, action string) error {
	if overridden() {
		return Permit(who, action, nil, nil)
	}
	if !who.HasRole("admin") {
		return model.Fail(model.Forbidden, "You are not authorized to complete "+action+" action.")
	}
	return nil
}

func (s *Service) stamp() time.Time { return s.now().UTC().Truncate(time.Second) }

func (s *Service) ListNamespaces(ctx context.Context, who auth.Identity, o model.NamespaceListOptions) ([]NamespaceDetail, error) {
	if err := who.Check(s.now()); err != nil {
		return nil, err
	}
	repo, err := s.metadefs()
	if err != nil {
		return nil, err
	}
	o.Project, o.Admin = who.ProjectID, who.HasRole("admin")
	list, err := repo.ListNamespaces(ctx, o)
	if errors.Is(err, repository.ErrMetadefNotFound) {
		return nil, model.Fail(model.NotFound, "Metadata definition namespace="+o.Marker+" was not found.")
	}
	if err != nil {
		return nil, err
	}
	out := make([]NamespaceDetail, 0, len(list))
	for _, n := range list {
		a, err := repo.Associations(ctx, n.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, NamespaceDetail{Namespace: n, Associations: a})
	}
	return out, nil
}

// GetNamespace embeds associations, properties, objects and tags. With a
// resource type, property names take its association prefix.
func (s *Service) GetNamespace(ctx context.Context, who auth.Identity, name, resourceType string) (NamespaceDetail, error) {
	repo, n, err := s.readNamespace(ctx, who, name)
	if err != nil {
		return NamespaceDetail{}, err
	}
	d := NamespaceDetail{Namespace: n}
	if d.Associations, err = repo.Associations(ctx, n.ID); err != nil {
		return d, err
	}
	if d.Properties, err = repo.Properties(ctx, n.ID); err != nil {
		return d, err
	}
	if d.Objects, err = repo.Objects(ctx, n.ID); err != nil {
		return d, err
	}
	if d.Tags, err = repo.Tags(ctx, n.ID); err != nil {
		return d, err
	}
	if resourceType != "" {
		for _, a := range d.Associations {
			if a.Name == resourceType && a.Prefix != nil {
				for i := range d.Properties {
					d.Properties[i].Name = *a.Prefix + d.Properties[i].Name
				}
				for i := range d.Objects {
					d.Objects[i] = prefixObject(d.Objects[i], *a.Prefix)
				}
			}
		}
	}
	return d, nil
}

func (s *Service) CreateNamespace(ctx context.Context, who auth.Identity, d NamespaceDetail) (NamespaceDetail, error) {
	if err := who.Check(s.now()); err != nil {
		return d, err
	}
	if err := metadefAdmin(who, "add_metadef_namespace"); err != nil {
		return d, err
	}
	repo, err := s.metadefs()
	if err != nil {
		return d, err
	}
	now := s.stamp()
	if d.Owner == "" {
		d.Owner = who.ProjectID
	}
	if d.Visibility == "" {
		d.Visibility = "private"
	}
	d.CreatedAt, d.UpdatedAt = now, &now
	for i := range d.Properties {
		d.Properties[i].CreatedAt = now
	}
	for i := range d.Objects {
		d.Objects[i].CreatedAt = now
	}
	for i := range d.Tags {
		d.Tags[i].CreatedAt = now
	}
	for i := range d.Associations {
		d.Associations[i].CreatedAt = now
	}
	n, err := repo.CreateNamespace(ctx, d.Namespace, d.Properties, d.Objects, d.Tags, d.Associations)
	if errors.Is(err, repository.ErrMetadefDuplicate) {
		return d, model.Fail(model.Conflict, "The metadata definition namespace="+d.Namespace.Namespace+" already exists.")
	}
	if err != nil {
		return d, err
	}
	d.Namespace = n
	s.notify("metadef_namespace.create", notify.NamespacePayload(n, ""))
	return d, nil
}

func (s *Service) UpdateNamespace(ctx context.Context, who auth.Identity, name string, n model.Namespace) (model.Namespace, error) {
	repo, current, err := s.writeNamespace(ctx, who, name, "modify_metadef_namespace")
	if err != nil {
		return n, err
	}
	now := s.stamp()
	n.ID, n.CreatedAt, n.UpdatedAt = current.ID, current.CreatedAt, &now
	if n.Owner == "" {
		n.Owner = current.Owner
	}
	if n.Visibility == "" { // a PUT replaces: the default, as the original
		n.Visibility = "private"
	}
	err = repo.UpdateNamespace(ctx, current.ID, n)
	if errors.Is(err, repository.ErrMetadefDuplicate) {
		return n, model.Fail(model.Conflict, "The metadata definition namespace="+n.Namespace+" already exists.")
	}
	if err != nil {
		return n, err
	}
	s.notify("metadef_namespace.update", notify.NamespacePayload(n, name))
	return n, nil
}

func protectedNamespace(n model.Namespace) error {
	if n.Protected {
		return model.Fail(model.Forbidden, "Metadata definition namespace "+n.Namespace+" is protected and cannot be deleted.")
	}
	return nil
}

func (s *Service) DeleteNamespace(ctx context.Context, who auth.Identity, name string) error {
	repo, n, err := s.writeNamespace(ctx, who, name, "delete_metadef_namespace")
	if err != nil {
		return err
	}
	if err = protectedNamespace(n); err != nil {
		return err
	}
	if err = repo.DeleteNamespace(ctx, n.ID); err != nil {
		return err
	}
	s.notify("metadef_namespace.delete", notify.NamespacePayload(n, ""))
	return nil
}

// DeleteChildren is DELETE /namespaces/{ns}/objects|properties|tags.
func (s *Service) DeleteChildren(ctx context.Context, who auth.Identity, name, kind string) error {
	repo, n, err := s.writeNamespace(ctx, who, name, "delete_metadef_namespace")
	if err != nil {
		return err
	}
	if err = protectedNamespace(n); err != nil {
		return err
	}
	switch kind {
	case "objects":
		err = repo.DeleteObjects(ctx, n.ID)
	case "properties":
		err = repo.DeleteProperties(ctx, n.ID)
	case "tags":
		err = repo.DeleteTags(ctx, n.ID)
	}
	if err == nil {
		s.notify("metadef_namespace.delete_"+kind, map[string]any{"namespace": n.Namespace})
	}
	return err
}

// Objects.

func objectNotFound(name string) error {
	return model.Fail(model.NotFound, "Could not find metadata object "+name)
}

func (s *Service) Objects(ctx context.Context, who auth.Identity, ns string) ([]model.MetadefObject, error) {
	repo, n, err := s.readNamespaceAs(ctx, who, ns, true)
	if err != nil {
		return nil, err
	}
	return repo.Objects(ctx, n.ID)
}

func (s *Service) Object(ctx context.Context, who auth.Identity, ns, name string) (model.MetadefObject, error) {
	repo, n, err := s.readNamespaceAs(ctx, who, ns, true)
	if err != nil {
		return model.MetadefObject{}, err
	}
	o, err := repo.Object(ctx, n.ID, name)
	if errors.Is(err, repository.ErrMetadefNotFound) {
		return o, objectNotFound(name)
	}
	return o, err
}

func (s *Service) SaveObject(ctx context.Context, who auth.Identity, ns, name string, o model.MetadefObject) (model.MetadefObject, error) {
	repo, n, err := s.writeNamespace(ctx, who, ns, map[bool]string{true: "add_metadef_object", false: "modify_metadef_object"}[name == ""])
	if err != nil {
		return o, err
	}
	now := s.stamp()
	if name == "" {
		o.CreatedAt, o.UpdatedAt = now, &now
		err = repo.CreateObject(ctx, n.ID, o)
	} else {
		current, e := repo.Object(ctx, n.ID, name)
		if errors.Is(e, repository.ErrMetadefNotFound) {
			return o, objectNotFound(name)
		}
		if e != nil {
			return o, e
		}
		o.CreatedAt, o.UpdatedAt = current.CreatedAt, &now
		err = repo.UpdateObject(ctx, n.ID, name, o)
	}
	if errors.Is(err, repository.ErrMetadefDuplicate) {
		return o, model.Fail(model.Conflict, "A metadata definition object with name="+o.Name+" already exists in namespace="+n.Namespace+".")
	}
	if errors.Is(err, repository.ErrMetadefNotFound) {
		return o, objectNotFound(name)
	}
	if err != nil {
		return o, err
	}
	event := "metadef_object.create"
	if name != "" {
		event = "metadef_object.update"
	}
	s.notify(event, map[string]any{"namespace": n.Namespace, "name": o.Name, "name_old": name})
	return o, nil
}

func (s *Service) DeleteObject(ctx context.Context, who auth.Identity, ns, name string) error {
	repo, n, err := s.writeNamespace(ctx, who, ns, "delete_metadef_object")
	if err != nil {
		return err
	}
	if n.Protected {
		return model.Fail(model.Forbidden, "Metadata definition object "+name+" is protected and cannot be deleted.")
	}
	if err = repo.DeleteObject(ctx, n.ID, name); errors.Is(err, repository.ErrMetadefNotFound) {
		return objectNotFound(name)
	}
	if err == nil {
		s.notify("metadef_object.delete", map[string]any{"namespace": n.Namespace, "name": name})
	}
	return err
}

// Properties.

func propertyNotFound(name string) error {
	return model.Fail(model.NotFound, "Could not find property "+name)
}

func (s *Service) Properties(ctx context.Context, who auth.Identity, ns string) ([]model.MetadefProperty, error) {
	repo, n, err := s.readNamespaceAs(ctx, who, ns, true)
	if err != nil {
		return nil, err
	}
	return repo.Properties(ctx, n.ID)
}

func (s *Service) Property(ctx context.Context, who auth.Identity, ns, name string) (model.MetadefProperty, error) {
	repo, n, err := s.readNamespaceAs(ctx, who, ns, true)
	if err != nil {
		return model.MetadefProperty{}, err
	}
	p, err := repo.Property(ctx, n.ID, name)
	if errors.Is(err, repository.ErrMetadefNotFound) {
		return p, propertyNotFound(name)
	}
	return p, err
}

func (s *Service) SaveProperty(ctx context.Context, who auth.Identity, ns, name string, p model.MetadefProperty) (model.MetadefProperty, error) {
	repo, n, err := s.writeNamespace(ctx, who, ns, map[bool]string{true: "add_metadef_property", false: "modify_metadef_property"}[name == ""])
	if err != nil {
		return p, err
	}
	now := s.stamp()
	if name == "" {
		p.CreatedAt, p.UpdatedAt = now, &now
		err = repo.CreateProperty(ctx, n.ID, p)
	} else {
		current, e := repo.Property(ctx, n.ID, name)
		if errors.Is(e, repository.ErrMetadefNotFound) {
			return p, propertyNotFound(name)
		}
		if e != nil {
			return p, e
		}
		p.CreatedAt, p.UpdatedAt = current.CreatedAt, &now
		err = repo.UpdateProperty(ctx, n.ID, name, p)
	}
	if errors.Is(err, repository.ErrMetadefDuplicate) {
		return p, model.Fail(model.Conflict, "A metadata definition property with name="+p.Name+" already exists in namespace="+n.Namespace+".")
	}
	if errors.Is(err, repository.ErrMetadefNotFound) {
		return p, propertyNotFound(name)
	}
	if err != nil {
		return p, err
	}
	event := "metadef_property.create"
	if name != "" {
		event = "metadef_property.update"
	}
	s.notify(event, map[string]any{"namespace": n.Namespace, "name": p.Name, "name_old": name})
	return p, nil
}

func (s *Service) DeleteProperty(ctx context.Context, who auth.Identity, ns, name string) error {
	repo, n, err := s.writeNamespace(ctx, who, ns, "remove_metadef_property")
	if err != nil {
		return err
	}
	if n.Protected {
		return model.Fail(model.Forbidden, "Metadata definition property "+name+" is protected and cannot be deleted.")
	}
	if err = repo.DeleteProperty(ctx, n.ID, name); errors.Is(err, repository.ErrMetadefNotFound) {
		return propertyNotFound(name)
	}
	if err == nil {
		s.notify("metadef_property.delete", map[string]any{"namespace": n.Namespace, "name": name})
	}
	return err
}

// Tags.

func tagNotFound(name string) error {
	return model.Fail(model.NotFound, "Could not find metadata tag "+name)
}

func (s *Service) MetadefTags(ctx context.Context, who auth.Identity, ns string) ([]model.MetadefTag, error) {
	repo, n, err := s.readNamespaceAs(ctx, who, ns, true)
	if err != nil {
		return nil, err
	}
	return repo.Tags(ctx, n.ID)
}

func (s *Service) MetadefTag(ctx context.Context, who auth.Identity, ns, name string) (model.MetadefTag, error) {
	repo, n, err := s.readNamespaceAs(ctx, who, ns, true)
	if err != nil {
		return model.MetadefTag{}, err
	}
	t, err := repo.Tag(ctx, n.ID, name)
	if errors.Is(err, repository.ErrMetadefNotFound) {
		return t, tagNotFound(name)
	}
	return t, err
}

func tagDuplicate(name, ns string) error {
	return model.Fail(model.Conflict, "A metadata tag with name="+name+" already exists in namespace="+ns+". (Please note that metadata tag names are case insensitive).")
}

// CreateMetadefTags is POST /tags (one or many; replace unless append).
func (s *Service) CreateMetadefTags(ctx context.Context, who auth.Identity, ns string, names []string, replace bool) ([]model.MetadefTag, error) {
	action := "add_metadef_tag"
	if len(names) != 1 || !replace {
		action = "add_metadef_tags"
	}
	repo, n, err := s.writeNamespace(ctx, who, ns, action)
	if err != nil {
		return nil, err
	}
	now := s.stamp()
	tags := make([]model.MetadefTag, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		if seen[strings.ToLower(name)] {
			return nil, tagDuplicate(name, n.Namespace)
		}
		seen[strings.ToLower(name)] = true
		tags = append(tags, model.MetadefTag{Name: name, CreatedAt: now, UpdatedAt: &now})
	}
	err = repo.CreateTags(ctx, n.ID, tags, replace)
	if errors.Is(err, repository.ErrMetadefDuplicate) {
		return nil, tagDuplicate(strings.Join(names, ","), n.Namespace)
	}
	if err != nil {
		return nil, err
	}
	for _, t := range tags {
		s.notify("metadef_tag.create", map[string]any{"namespace": n.Namespace, "name": t.Name})
	}
	return tags, nil
}

func (s *Service) UpdateMetadefTag(ctx context.Context, who auth.Identity, ns, name, newName string) (model.MetadefTag, error) {
	repo, n, err := s.writeNamespace(ctx, who, ns, "modify_metadef_tag")
	if err != nil {
		return model.MetadefTag{}, err
	}
	current, err := repo.Tag(ctx, n.ID, name)
	if errors.Is(err, repository.ErrMetadefNotFound) {
		return current, tagNotFound(name)
	}
	if err != nil {
		return current, err
	}
	now := s.stamp()
	t := model.MetadefTag{Name: newName, CreatedAt: current.CreatedAt, UpdatedAt: &now}
	if err = repo.UpdateTag(ctx, n.ID, name, t); errors.Is(err, repository.ErrMetadefDuplicate) {
		return t, tagDuplicate(newName, n.Namespace)
	}
	if err == nil {
		s.notify("metadef_tag.update", map[string]any{"namespace": n.Namespace, "name": newName, "name_old": name})
	}
	return t, err
}

func (s *Service) DeleteMetadefTag(ctx context.Context, who auth.Identity, ns, name string) error {
	repo, n, err := s.writeNamespace(ctx, who, ns, "delete_metadef_tag")
	if err != nil {
		return err
	}
	if n.Protected {
		return model.Fail(model.Forbidden, "Metadata definition tag "+name+" is protected and cannot be deleted.")
	}
	if err = repo.DeleteTag(ctx, n.ID, name); errors.Is(err, repository.ErrMetadefNotFound) {
		return tagNotFound(name)
	}
	if err == nil {
		s.notify("metadef_tag.delete", map[string]any{"namespace": n.Namespace, "name": name})
	}
	return err
}

// Resource types.

func (s *Service) ResourceTypes(ctx context.Context, who auth.Identity) ([]model.ResourceType, error) {
	if err := who.Check(s.now()); err != nil {
		return nil, err
	}
	repo, err := s.metadefs()
	if err != nil {
		return nil, err
	}
	return repo.ResourceTypes(ctx)
}

func (s *Service) Associations(ctx context.Context, who auth.Identity, ns string) ([]model.ResourceTypeAssociation, error) {
	repo, n, err := s.readNamespaceAs(ctx, who, ns, true)
	if err != nil {
		return nil, err
	}
	return repo.Associations(ctx, n.ID)
}

func (s *Service) Associate(ctx context.Context, who auth.Identity, ns string, a model.ResourceTypeAssociation) (model.ResourceTypeAssociation, error) {
	repo, n, err := s.writeNamespace(ctx, who, ns, "add_metadef_resource_type_association")
	if err != nil {
		return a, err
	}
	now := s.stamp()
	a.CreatedAt, a.UpdatedAt = now, &now
	if err = repo.Associate(ctx, n.ID, a); errors.Is(err, repository.ErrMetadefDuplicate) {
		return a, model.Fail(model.Conflict, "The metadata definition resource-type association of resource-type="+a.Name+" to namespace="+n.Namespace+", already exists.")
	}
	if err == nil {
		s.notify("metadef_resource_type.create", map[string]any{"namespace": n.Namespace, "name": a.Name})
	}
	return a, err
}

func (s *Service) Dissociate(ctx context.Context, who auth.Identity, ns, name string) error {
	repo, n, err := s.writeNamespace(ctx, who, ns, "remove_metadef_resource_type_association")
	if err != nil {
		return err
	}
	if n.Protected {
		return model.Fail(model.Forbidden, "Metadata definition resource-type-association "+name+" is protected and cannot be deleted.")
	}
	if err = repo.Dissociate(ctx, n.ID, name); errors.Is(err, repository.ErrMetadefNotFound) {
		return model.Fail(model.NotFound, "The metadata definition resource-type association of resource-type="+name+" to namespace="+n.Namespace+", was not found.")
	}
	if err == nil {
		s.notify("metadef_resource_type.delete", map[string]any{"namespace": n.Namespace, "name": name})
	}
	return err
}

func prefixObject(o model.MetadefObject, prefix string) model.MetadefObject {
	props := map[string]json.RawMessage{}
	if json.Unmarshal(o.Properties, &props) == nil {
		out := map[string]json.RawMessage{}
		for k, v := range props {
			out[prefix+k] = v
		}
		o.Properties, _ = json.Marshal(out)
	}
	required := make([]string, 0, len(o.Required))
	for _, r := range o.Required {
		required = append(required, prefix+r)
	}
	if o.Required != nil {
		o.Required = required
	}
	return o
}
