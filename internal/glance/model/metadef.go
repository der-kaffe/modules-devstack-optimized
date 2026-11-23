package model

import (
	"encoding/json"
	"time"
)

// Metadata definitions (metadefs): the original's metadef_* tables.

type Namespace struct {
	ID                       int64
	Namespace                string
	DisplayName, Description *string
	Visibility               string
	Protected                bool
	Owner                    string
	CreatedAt                time.Time
	UpdatedAt                *time.Time
}

type MetadefObject struct {
	Name        string
	Description *string
	Required    []string
	Properties  json.RawMessage // json_schema: {name: property schema}
	CreatedAt   time.Time
	UpdatedAt   *time.Time
}

type MetadefProperty struct {
	Name      string
	Schema    json.RawMessage // json_schema, without the name
	CreatedAt time.Time
	UpdatedAt *time.Time
}

type MetadefTag struct {
	Name      string
	CreatedAt time.Time
	UpdatedAt *time.Time
}

type ResourceType struct {
	Name      string
	Protected bool
	CreatedAt time.Time
	UpdatedAt *time.Time
}

type ResourceTypeAssociation struct {
	Name                     string
	Prefix, PropertiesTarget *string
	CreatedAt                time.Time
	UpdatedAt                *time.Time
}

// NamespaceListOptions are GET /v2/metadefs/namespaces parameters.
type NamespaceListOptions struct {
	Limit            int
	Marker           string
	SortKey, SortDir string
	Visibility       string
	ResourceTypes    []string
	Project          string
	Admin            bool
}
