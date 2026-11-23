package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
)

// Metadata definitions, as the original's metadef_* controllers: the same
// routes, representations (null fields omitted, updated_at only once set)
// and messages.

const metadefBase = "/v2/metadefs/namespaces/"

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func setTimes(out map[string]any, created time.Time, updated *time.Time) {
	out["created_at"] = stamp(created)
	if updated != nil {
		out["updated_at"] = stamp(*updated)
	}
}

func associationRep(a model.ResourceTypeAssociation) map[string]any {
	out := map[string]any{"name": a.Name}
	if a.Prefix != nil {
		out["prefix"] = *a.Prefix
	}
	if a.PropertiesTarget != nil {
		out["properties_target"] = *a.PropertiesTarget
	}
	setTimes(out, a.CreatedAt, a.UpdatedAt)
	return out
}

func propertiesRep(props []model.MetadefProperty) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, p := range props {
		out[p.Name] = textDefault(p.Schema)
	}
	return out
}

// textDefault: the original's property type holds "default" as text, so a
// numeric default stored as a JSON number is returned as a string.
func textDefault(schema json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(schema, &fields) != nil {
		return schema
	}
	d, ok := fields["default"]
	if !ok || len(d) == 0 || d[0] == '"' || d[0] == '[' || d[0] == '{' || string(d) == "null" || string(d) == "true" || string(d) == "false" {
		return schema
	}
	fields["default"], _ = json.Marshal(string(d))
	out, _ := json.Marshal(fields)
	return out
}

// objectProperties applies textDefault to each property of an object.
func objectProperties(raw json.RawMessage) map[string]json.RawMessage {
	props := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &props)
	for k, v := range props {
		props[k] = textDefault(v)
	}
	return props
}

// objectRep: a read shows required and properties (empty when unset); a
// create or update response omits them when empty, as the original.
func objectRep(ns string, o model.MetadefObject, written ...bool) map[string]any {
	echo := len(written) > 0 && written[0]
	out := map[string]any{"name": o.Name, "self": metadefBase + ns + "/objects/" + o.Name, "schema": "/v2/schemas/metadefs/object"}
	if o.Required != nil || !echo {
		if o.Required == nil {
			o.Required = []string{}
		}
		out["required"] = o.Required
	}
	if props := objectProperties(o.Properties); len(props) > 0 || !echo {
		out["properties"] = props
	}
	if o.Description != nil {
		out["description"] = *o.Description
	}
	setTimes(out, o.CreatedAt, o.UpdatedAt)
	return out
}

func tagRep(t model.MetadefTag) map[string]any {
	out := map[string]any{"name": t.Name}
	setTimes(out, t.CreatedAt, t.UpdatedAt)
	return out
}

func propertyRep(p model.MetadefProperty) any {
	out := map[string]any{}
	_ = json.Unmarshal(textDefault(p.Schema), &out)
	out["name"] = p.Name
	return out
}

// namespaceRep: full embeds the children (GET); created echoes them as the
// original's create response does, without their timestamps or links.
func namespaceRep(d service.NamespaceDetail, full bool, created ...bool) map[string]any {
	echo := len(created) > 0 && created[0]
	n := d.Namespace
	out := map[string]any{"namespace": n.Namespace, "visibility": n.Visibility, "protected": n.Protected, "owner": n.Owner,
		"self": metadefBase + n.Namespace, "schema": "/v2/schemas/metadefs/namespace"}
	if n.DisplayName != nil {
		out["display_name"] = *n.DisplayName
	}
	if n.Description != nil {
		out["description"] = *n.Description
	}
	setTimes(out, n.CreatedAt, n.UpdatedAt)
	if len(d.Associations) > 0 {
		list := make([]map[string]any, 0, len(d.Associations))
		for _, a := range d.Associations {
			rep := associationRep(a)
			if echo {
				delete(rep, "created_at")
				delete(rep, "updated_at")
			}
			list = append(list, rep)
		}
		out["resource_type_associations"] = list
	}
	if !full {
		return out
	}
	if len(d.Properties) > 0 {
		out["properties"] = propertiesRep(d.Properties)
	}
	if len(d.Objects) > 0 {
		list := make([]map[string]any, 0, len(d.Objects))
		for _, o := range d.Objects {
			rep := objectRep(n.Namespace, o, echo)
			if echo {
				for _, k := range []string{"self", "schema", "created_at", "updated_at"} {
					delete(rep, k)
				}
			}
			list = append(list, rep)
		}
		out["objects"] = list
	}
	if len(d.Tags) > 0 {
		list := make([]map[string]any, 0, len(d.Tags))
		for _, t := range d.Tags {
			list = append(list, map[string]any{"name": t.Name})
		}
		out["tags"] = list
	}
	return out
}

func schemaError(name, detail string) error {
	return model.Fail(model.Invalid, "Provided object does not match schema '"+name+"': "+detail)
}

func maxLen(name, field string, v *string, n int) error {
	if v != nil && (utf8.RuneCountInString(*v) > n) {
		return schemaError(name, fmt.Sprintf("'%s' is too long", field))
	}
	return nil
}

var propertyKeys = []string{"name", "title", "description", "operators", "type", "required", "minimum", "maximum", "maxLength", "minLength",
	"pattern", "enum", "readonly", "default", "items", "maxItems", "minItems", "uniqueItems", "additionalItems"}

// propertySchema validates one property definition against the original's
// property schema and returns its stored json_schema (without the name).
func propertySchema(raw json.RawMessage, named bool) (string, json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "", nil, schemaError("property", "property must be an object")
	}
	var name string
	if named {
		if json.Unmarshal(fields["name"], &name) != nil || name == "" || utf8.RuneCountInString(name) > 80 {
			return "", nil, schemaError("property", "'name' is a required property")
		}
	}
	for k := range fields {
		if !slices.Contains(propertyKeys, k) {
			return "", nil, schemaError("property", fmt.Sprintf("Additional properties are not allowed ('%s' was unexpected)", k))
		}
	}
	var title string
	if json.Unmarshal(fields["title"], &title) != nil {
		return "", nil, schemaError("property", "'title' is a required property")
	}
	var kind *string
	if t, ok := fields["type"]; !ok || json.Unmarshal(t, &kind) != nil || kind != nil && !slices.Contains([]string{"array", "boolean", "integer", "number", "object", "string"}, *kind) {
		return "", nil, schemaError("property", "'type' is a required property with a valid value")
	}
	if !named { // a property of a namespace or object body: stored as given
		delete(fields, "name")
	}
	stored, _ := json.Marshal(fields)
	return name, stored, nil
}

type namespaceBody struct {
	Namespace   *string                    `json:"namespace"`
	DisplayName *string                    `json:"display_name"`
	Description *string                    `json:"description"`
	Visibility  *string                    `json:"visibility"`
	Protected   *bool                      `json:"protected"`
	Owner       *string                    `json:"owner"`
	Properties  map[string]json.RawMessage `json:"properties"`
	Objects     []json.RawMessage          `json:"objects"`
	Tags        []struct {
		Name string `json:"name"`
	} `json:"tags"`
	Associations []struct {
		Name             string  `json:"name"`
		Prefix           *string `json:"prefix"`
		PropertiesTarget *string `json:"properties_target"`
	} `json:"resource_type_associations"`
}

var namespaceKeys = []string{"namespace", "display_name", "description", "visibility", "protected", "owner", "properties", "objects", "tags",
	"resource_type_associations", "created_at", "updated_at", "self", "schema"}

func decodeNamespace(raw map[string]json.RawMessage) (service.NamespaceDetail, error) {
	var d service.NamespaceDetail
	for k := range raw {
		if !slices.Contains(namespaceKeys, k) {
			return d, schemaError("namespace", fmt.Sprintf("Additional properties are not allowed ('%s' was unexpected)", k))
		}
	}
	data, _ := json.Marshal(raw)
	var b namespaceBody
	if json.Unmarshal(data, &b) != nil {
		return d, schemaError("namespace", "invalid value")
	}
	if b.Namespace == nil || *b.Namespace == "" {
		return d, schemaError("namespace", "'namespace' is a required property")
	}
	for _, e := range []error{maxLen("namespace", "namespace", b.Namespace, 80), maxLen("namespace", "display_name", b.DisplayName, 80),
		maxLen("namespace", "description", b.Description, 500), maxLen("namespace", "owner", b.Owner, 255)} {
		if e != nil {
			return d, e
		}
	}
	d.Namespace.Namespace, d.DisplayName, d.Description = *b.Namespace, b.DisplayName, b.Description
	if b.Visibility != nil {
		if *b.Visibility != "public" && *b.Visibility != "private" {
			return d, schemaError("namespace", "'"+*b.Visibility+"' is not one of ['public', 'private']")
		}
		d.Visibility = *b.Visibility
	}
	if b.Protected != nil {
		d.Protected = *b.Protected
	}
	if b.Owner != nil {
		d.Owner = *b.Owner
	}
	for name, p := range b.Properties {
		_, stored, err := propertySchema(p, false)
		if err != nil {
			return d, err
		}
		d.Properties = append(d.Properties, model.MetadefProperty{Name: name, Schema: stored})
	}
	for _, o := range b.Objects {
		obj, err := decodeObject(o)
		if err != nil {
			return d, err
		}
		d.Objects = append(d.Objects, obj)
	}
	for _, t := range b.Tags {
		if t.Name == "" || utf8.RuneCountInString(t.Name) > 80 {
			return d, schemaError("tag", "'name' is a required property")
		}
		d.Tags = append(d.Tags, model.MetadefTag{Name: t.Name})
	}
	for _, a := range b.Associations {
		if a.Name == "" || utf8.RuneCountInString(a.Name) > 80 {
			return d, schemaError("resource_type_association", "'name' is a required property")
		}
		d.Associations = append(d.Associations, model.ResourceTypeAssociation{Name: a.Name, Prefix: a.Prefix, PropertiesTarget: a.PropertiesTarget})
	}
	return d, nil
}

func decodeObject(raw json.RawMessage) (model.MetadefObject, error) {
	var fields map[string]json.RawMessage
	var o model.MetadefObject
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return o, schemaError("object", "object must be an object")
	}
	for k := range fields {
		if !slices.Contains([]string{"name", "description", "required", "properties", "created_at", "updated_at", "self", "schema"}, k) {
			return o, schemaError("object", fmt.Sprintf("Additional properties are not allowed ('%s' was unexpected)", k))
		}
	}
	if json.Unmarshal(fields["name"], &o.Name) != nil || o.Name == "" || utf8.RuneCountInString(o.Name) > 80 {
		return o, schemaError("object", "'name' is a required property")
	}
	if d, ok := fields["description"]; ok && json.Unmarshal(d, &o.Description) != nil {
		return o, schemaError("object", "'description' must be a string")
	}
	if r, ok := fields["required"]; ok && json.Unmarshal(r, &o.Required) != nil {
		return o, schemaError("object", "'required' must be an array of strings")
	}
	props := map[string]json.RawMessage{}
	if p, ok := fields["properties"]; ok && json.Unmarshal(p, &props) != nil {
		return o, schemaError("object", "'properties' must be an object")
	}
	clean := map[string]json.RawMessage{}
	for name, p := range props {
		_, stored, err := propertySchema(p, false)
		if err != nil {
			return o, err
		}
		clean[name] = stored
	}
	o.Properties, _ = json.Marshal(clean)
	return o, nil
}

func (h *Handler) rawBody(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	var raw map[string]json.RawMessage
	if !h.readObject(w, r, &raw) {
		return nil, false
	}
	return raw, true
}

func (h *Handler) namespaces(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		o := model.NamespaceListOptions{Limit: 25, Marker: q.Get("marker"), SortKey: firstNonEmpty(q.Get("sort_key"), "created_at"),
			SortDir: firstNonEmpty(q.Get("sort_dir"), "desc"), Visibility: q.Get("visibility")}
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				h.status(w, 400, "limit param must be an integer")
				return
			}
			o.Limit = min(n, 1000)
		}
		if !slices.Contains([]string{"created_at", "updated_at", "namespace", "id"}, o.SortKey) || o.SortDir != "asc" && o.SortDir != "desc" {
			h.status(w, 400, "Invalid sort key or direction.")
			return
		}
		if o.Visibility != "" && o.Visibility != "public" && o.Visibility != "private" {
			h.status(w, 400, "Invalid visibility value: "+o.Visibility)
			return
		}
		if v := q.Get("resource_types"); v != "" {
			for _, t := range strings.Split(v, ",") {
				o.ResourceTypes = append(o.ResourceTypes, strings.TrimSpace(t))
			}
		}
		list, err := h.service.ListNamespaces(r.Context(), who(r), o)
		if err != nil {
			h.failure(w, err)
			return
		}
		out := make([]map[string]any, 0, len(list))
		for _, d := range list {
			out = append(out, namespaceRep(d, false))
		}
		first := "/v2/metadefs/namespaces"
		if e := withoutMarker(r.URL.RawQuery); e != "" {
			first += "?" + e
		}
		q.Del("marker")
		body := map[string]any{"namespaces": out, "first": first, "schema": "/v2/schemas/metadefs/namespaces"}
		if o.Limit > 0 && len(list) == o.Limit {
			body["next"] = nextLink("/v2/metadefs/namespaces", r.URL.RawQuery, list[len(list)-1].Namespace.Namespace)
		}
		h.json(w, 200, body)
	case http.MethodPost:
		raw, ok := h.rawBody(w, r)
		if !ok {
			return
		}
		d, err := decodeNamespace(raw)
		if err == nil {
			d, err = h.service.CreateNamespace(r.Context(), who(r), d)
		}
		if err != nil {
			h.failure(w, err)
			return
		}
		h.json(w, 201, namespaceRep(d, true, true))
	default:
		w.Header().Set("Allow", "GET, POST")
		h.status(w, 405, "Method not implemented.")
	}
}

// withoutMarker re-encodes a raw query in the client's order without
// marker, as the original's links (urlencode of the request parameters).
func withoutMarker(raw string) string {
	parts := []string{}
	for _, p := range strings.Split(raw, "&") {
		k, v, _ := strings.Cut(p, "=")
		key, err1 := url.QueryUnescape(k)
		value, err2 := url.QueryUnescape(v)
		if p == "" || key == "marker" || err1 != nil || err2 != nil {
			continue
		}
		parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(value))
	}
	return strings.Join(parts, "&")
}

func nextLink(base, raw, marker string) string {
	q := withoutMarker(raw)
	if q != "" {
		q += "&"
	}
	return base + "?" + q + "marker=" + url.QueryEscape(marker)
}

func firstNonEmpty(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func (h *Handler) namespace(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	switch r.Method {
	case http.MethodGet:
		d, err := h.service.GetNamespace(r.Context(), who(r), ns, r.URL.Query().Get("resource_type"))
		if err != nil {
			h.failure(w, err)
			return
		}
		h.json(w, 200, namespaceRep(d, true))
	case http.MethodPut:
		raw, ok := h.rawBody(w, r)
		if !ok {
			return
		}
		d, err := decodeNamespace(raw)
		if err == nil {
			d.Namespace, err = h.service.UpdateNamespace(r.Context(), who(r), ns, d.Namespace)
		}
		if err != nil {
			h.failure(w, err)
			return
		}
		h.json(w, 200, namespaceRep(service.NamespaceDetail{Namespace: d.Namespace}, false))
	case http.MethodDelete:
		if err := h.service.DeleteNamespace(r.Context(), who(r), ns); err != nil {
			h.failure(w, err)
			return
		}
		w.WriteHeader(204)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		h.status(w, 405, "Method not implemented.")
	}
}

func (h *Handler) deleteChildren(w http.ResponseWriter, r *http.Request, kind string) {
	if err := h.service.DeleteChildren(r.Context(), who(r), r.PathValue("ns"), kind); err != nil {
		h.failure(w, err)
		return
	}
	w.WriteHeader(204)
}

func (h *Handler) metadefObjects(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	switch r.Method {
	case http.MethodGet:
		list, err := h.service.Objects(r.Context(), who(r), ns)
		if err != nil {
			h.failure(w, err)
			return
		}
		out := make([]map[string]any, 0, len(list))
		for _, o := range list {
			out = append(out, objectRep(ns, o))
		}
		h.json(w, 200, map[string]any{"objects": out, "schema": "v2/schemas/metadefs/objects"})
	case http.MethodPost:
		h.saveObject(w, r, ns, "", 201)
	case http.MethodDelete:
		h.deleteChildren(w, r, "objects")
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		h.status(w, 405, "Method not implemented.")
	}
}

func (h *Handler) saveObject(w http.ResponseWriter, r *http.Request, ns, name string, code int) {
	raw, ok := h.rawBody(w, r)
	if !ok {
		return
	}
	data, _ := json.Marshal(raw)
	o, err := decodeObject(data)
	if err == nil {
		o, err = h.service.SaveObject(r.Context(), who(r), ns, name, o)
	}
	if err != nil {
		h.failure(w, err)
		return
	}
	h.json(w, code, objectRep(ns, o, true))
}

func (h *Handler) metadefObject(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	switch r.Method {
	case http.MethodGet:
		o, err := h.service.Object(r.Context(), who(r), ns, name)
		if err != nil {
			h.failure(w, err)
			return
		}
		h.json(w, 200, objectRep(ns, o))
	case http.MethodPut:
		h.saveObject(w, r, ns, name, 200)
	case http.MethodDelete:
		if err := h.service.DeleteObject(r.Context(), who(r), ns, name); err != nil {
			h.failure(w, err)
			return
		}
		w.WriteHeader(204)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		h.status(w, 405, "Method not implemented.")
	}
}

func (h *Handler) metadefProperties(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	switch r.Method {
	case http.MethodGet:
		list, err := h.service.Properties(r.Context(), who(r), ns)
		if err != nil {
			h.failure(w, err)
			return
		}
		h.json(w, 200, map[string]any{"properties": propertiesRep(list)})
	case http.MethodPost:
		h.saveProperty(w, r, ns, "", 201)
	case http.MethodDelete:
		h.deleteChildren(w, r, "properties")
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		h.status(w, 405, "Method not implemented.")
	}
}

func (h *Handler) saveProperty(w http.ResponseWriter, r *http.Request, ns, name string, code int) {
	raw, ok := h.rawBody(w, r)
	if !ok {
		return
	}
	data, _ := json.Marshal(raw)
	newName, schema, err := propertySchema(data, true)
	var p model.MetadefProperty
	if err == nil {
		p, err = h.service.SaveProperty(r.Context(), who(r), ns, name, model.MetadefProperty{Name: newName, Schema: schema})
	}
	if err != nil {
		h.failure(w, err)
		return
	}
	h.json(w, code, propertyRep(p))
}

func (h *Handler) metadefProperty(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	switch r.Method {
	case http.MethodGet:
		p, err := h.service.Property(r.Context(), who(r), ns, name)
		if err != nil {
			h.failure(w, err)
			return
		}
		h.json(w, 200, propertyRep(p))
	case http.MethodPut:
		h.saveProperty(w, r, ns, name, 200)
	case http.MethodDelete:
		if err := h.service.DeleteProperty(r.Context(), who(r), ns, name); err != nil {
			h.failure(w, err)
			return
		}
		w.WriteHeader(204)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		h.status(w, 405, "Method not implemented.")
	}
}

func (h *Handler) metadefTags(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	switch r.Method {
	case http.MethodGet:
		list, err := h.service.MetadefTags(r.Context(), who(r), ns)
		if err != nil {
			h.failure(w, err)
			return
		}
		out := make([]map[string]any, 0, len(list))
		for _, t := range list {
			out = append(out, map[string]any{"name": t.Name})
		}
		h.json(w, 200, map[string]any{"tags": out})
	case http.MethodPost:
		var body struct {
			Tags []struct {
				Name string `json:"name"`
			} `json:"tags"`
		}
		if !h.readObject(w, r, &body) {
			return
		}
		names := []string{}
		for _, t := range body.Tags {
			if t.Name == "" || utf8.RuneCountInString(t.Name) > 80 {
				h.failure(w, schemaError("tags", "'name' is a required property"))
				return
			}
			names = append(names, t.Name)
		}
		replace := !strings.EqualFold(r.Header.Get("X-Openstack-Append"), "true")
		tags, err := h.service.CreateMetadefTags(r.Context(), who(r), ns, names, replace)
		if err != nil {
			h.failure(w, err)
			return
		}
		out := make([]map[string]any, 0, len(tags))
		for _, t := range tags {
			out = append(out, map[string]any{"name": t.Name})
		}
		h.json(w, 201, map[string]any{"tags": out})
	case http.MethodDelete:
		h.deleteChildren(w, r, "tags")
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		h.status(w, 405, "Method not implemented.")
	}
}

func (h *Handler) metadefTag(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	switch r.Method {
	case http.MethodGet:
		t, err := h.service.MetadefTag(r.Context(), who(r), ns, name)
		if err != nil {
			h.failure(w, err)
			return
		}
		h.json(w, 200, tagRep(t))
	case http.MethodPost:
		if utf8.RuneCountInString(name) > 80 {
			h.failure(w, schemaError("tag", "'name' is too long"))
			return
		}
		tags, err := h.service.CreateMetadefTags(r.Context(), who(r), ns, []string{name}, false)
		if err != nil {
			h.failure(w, err)
			return
		}
		h.json(w, 201, tagRep(tags[0]))
	case http.MethodPut:
		var body struct {
			Name string `json:"name"`
		}
		if !h.readObject(w, r, &body) {
			return
		}
		if body.Name == "" || utf8.RuneCountInString(body.Name) > 80 {
			h.failure(w, schemaError("tag", "'name' is a required property"))
			return
		}
		t, err := h.service.UpdateMetadefTag(r.Context(), who(r), ns, name, body.Name)
		if err != nil {
			h.failure(w, err)
			return
		}
		h.json(w, 200, tagRep(t))
	case http.MethodDelete:
		if err := h.service.DeleteMetadefTag(r.Context(), who(r), ns, name); err != nil {
			h.failure(w, err)
			return
		}
		w.WriteHeader(204)
	default:
		w.Header().Set("Allow", "GET, POST, PUT, DELETE")
		h.status(w, 405, "Method not implemented.")
	}
}

func (h *Handler) resourceTypes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		h.status(w, 405, "Method not implemented.")
		return
	}
	list, err := h.service.ResourceTypes(r.Context(), who(r))
	if err != nil {
		h.failure(w, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		rep := map[string]any{"name": t.Name}
		setTimes(rep, t.CreatedAt, t.UpdatedAt)
		out = append(out, rep)
	}
	h.json(w, 200, map[string]any{"resource_types": out})
}

func (h *Handler) namespaceResourceTypes(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	switch r.Method {
	case http.MethodGet:
		list, err := h.service.Associations(r.Context(), who(r), ns)
		if err != nil {
			h.failure(w, err)
			return
		}
		out := make([]map[string]any, 0, len(list))
		for _, a := range list {
			out = append(out, associationRep(a))
		}
		h.json(w, 200, map[string]any{"resource_type_associations": out})
	case http.MethodPost:
		var body struct {
			Name             string  `json:"name"`
			Prefix           *string `json:"prefix"`
			PropertiesTarget *string `json:"properties_target"`
		}
		if !h.readObject(w, r, &body) {
			return
		}
		if body.Name == "" || utf8.RuneCountInString(body.Name) > 80 {
			h.failure(w, schemaError("resource_type_association", "'name' is a required property"))
			return
		}
		a, err := h.service.Associate(r.Context(), who(r), ns, model.ResourceTypeAssociation{Name: body.Name, Prefix: body.Prefix, PropertiesTarget: body.PropertiesTarget})
		if err != nil {
			h.failure(w, err)
			return
		}
		h.json(w, 201, associationRep(a))
	default:
		w.Header().Set("Allow", "GET, POST")
		h.status(w, 405, "Method not implemented.")
	}
}

func (h *Handler) namespaceResourceType(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		h.status(w, 405, "Method not implemented.")
		return
	}
	if err := h.service.Dissociate(r.Context(), who(r), r.PathValue("ns"), r.PathValue("name")); err != nil {
		h.failure(w, err)
		return
	}
	w.WriteHeader(204)
}
