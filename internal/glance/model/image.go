// Package model defines the metadata-only image domain for B20 delivery A.
package model

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type Kind uint8

const (
	Invalid Kind = iota + 1
	Forbidden
	NotFound
	Conflict
	LimitExceeded
	// Unsupported is the original's InvalidImageData (415).
	Unsupported
)

type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return e.Message }

func Fail(kind Kind, message string) error { return &Error{Kind: kind, Message: message} }

// Pointer fields preserve a nullable value. HTTP decoding separately retains
// presence so that explicit null is rejected for non-nullable input fields.
type CreateInput struct {
	ID, Name, DiskFormat, ContainerFormat, Visibility, Owner *string
	MinDisk, MinRAM                                          *int64
	Protected, Hidden                                        *bool
	Tags                                                     []string
	Properties                                               map[string]string
}

type Image struct {
	ID, Owner, Status, Visibility      string
	Name, DiskFormat, ContainerFormat  *string
	MinDisk, MinRAM                    int64
	Protected, Hidden                  bool
	Size, VirtualSize                  *int64
	Checksum, HashAlgorithm, HashValue *string
	CreatedAt, UpdatedAt               time.Time
	Tags                               []string
	Properties                         map[string]string
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func CanonicalID(value string) (string, error) {
	if !uuidPattern.MatchString(value) {
		return "", Fail(Invalid, "Image ID must be a UUID in canonical hyphenated form.")
	}
	id, err := uuid.Parse(value)
	if err != nil {
		return "", Fail(Invalid, "Invalid image UUID.")
	}
	return id.String(), nil
}

func member(value string, choices string) bool {
	for _, choice := range strings.Fields(choices) {
		if value == choice {
			return true
		}
	}
	return false
}

func New(input CreateInput, owner string, now time.Time) (Image, error) {
	image := Image{Owner: owner, Status: "queued", Visibility: "shared",
		Name: input.Name, DiskFormat: input.DiskFormat, ContainerFormat: input.ContainerFormat,
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(), Tags: []string{}, Properties: map[string]string{}}
	if owner == "" || utf8.RuneCountInString(owner) > 255 {
		return Image{}, Fail(Invalid, "Invalid project owner.")
	}
	if input.Owner != nil && *input.Owner != owner {
		return Image{}, Fail(Forbidden, "Owner must match the authenticated project.")
	}
	if input.ID != nil {
		id, err := CanonicalID(*input.ID)
		if err != nil {
			return Image{}, err
		}
		image.ID = id
	} else {
		id, err := uuid.NewRandom()
		if err != nil {
			return Image{}, fmt.Errorf("generate image UUID: %w", err)
		}
		image.ID = id.String()
	}
	if input.Name != nil && utf8.RuneCountInString(*input.Name) > 255 {
		return Image{}, Fail(Invalid, "Image name exceeds 255 characters.")
	}
	if input.Visibility != nil {
		image.Visibility = *input.Visibility
	}
	if !member(image.Visibility, "private public shared community") {
		return Image{}, Fail(Invalid, "Invalid image visibility.")
	}
	if input.DiskFormat != nil && !member(*input.DiskFormat, "ami ari aki vhd vhdx vmdk raw qcow2 vdi iso ploop") {
		return Image{}, Fail(Invalid, "Invalid disk_format.")
	}
	if input.ContainerFormat != nil && !member(*input.ContainerFormat, "ami ari aki bare ovf ova docker compressed") {
		return Image{}, Fail(Invalid, "Invalid container_format.")
	}
	if input.MinDisk != nil {
		image.MinDisk = *input.MinDisk
	}
	if input.MinRAM != nil {
		image.MinRAM = *input.MinRAM
	}
	if image.MinDisk < 0 || image.MinRAM < 0 || image.MinDisk > math.MaxInt32 || image.MinRAM > math.MaxInt32 {
		return Image{}, Fail(Invalid, "min_disk and min_ram must be non-negative 32-bit integers.")
	}
	if input.Protected != nil {
		image.Protected = *input.Protected
	}
	if input.Hidden != nil {
		image.Hidden = *input.Hidden
	}
	seen := map[string]bool{}
	for _, tag := range input.Tags {
		if utf8.RuneCountInString(tag) > 255 {
			return Image{}, Fail(Invalid, "Tag exceeds 255 characters.")
		}
		if !seen[tag] {
			image.Tags = append(image.Tags, tag)
			seen[tag] = true
		}
	}
	if Over(len(image.Tags), Quotas.Tags) || Over(len(input.Properties), Quotas.Properties) {
		return Image{}, Fail(LimitExceeded, "Image metadata quota exceeded.")
	}
	for key, value := range input.Properties {
		if Reserved(key) {
			return Image{}, Fail(Forbidden, "Property is reserved: "+key)
		}
		if utf8.RuneCountInString(key) > 255 || len(value) > 65535 {
			return Image{}, Fail(Invalid, "Image property exceeds the supported SQL length.")
		}
		image.Properties[key] = value
	}
	return image, nil
}

func Reserved(key string) bool {
	return strings.HasPrefix(key, "os_glance") || member(key,
		"id name owner status visibility disk_format container_format size virtual_size checksum os_hash_algo os_hash_value created_at updated_at min_disk min_ram protected os_hidden tags self file schema direct_url stores location locations deleted deleted_at")
}
