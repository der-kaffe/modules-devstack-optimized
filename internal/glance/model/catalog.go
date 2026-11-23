package model

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const DefaultLimit = 25
const MaxLimit = 1000

type Sort struct{ Key, Direction string }

// ProjectID and Admin are filled by the service from a verified identity.
type ListOptions struct {
	Limit        int
	Marker       string
	Sort         []Sort
	TieDirection string
	Filters      map[string]string
	Tags         []string
	ProjectID    string
	Admin        bool
	// MemberStatus selects shared images by the project's membership status:
	// accepted (default), pending, rejected or all.
	MemberStatus string
}

func Sortable(key string) bool {
	return member(key, "name status container_format disk_format size id created_at updated_at")
}

func (o ListOptions) Validate() (ListOptions, error) {
	bad := func() (ListOptions, error) { return o, Fail(Invalid, "Invalid or unsupported list parameter.") }
	if o.Limit < 0 || len(o.Sort) > 8 || len(o.Filters) > 128 || len(o.Tags) > 128 {
		return bad()
	}
	if o.Limit > MaxLimit {
		o.Limit = MaxLimit
	}
	if o.Marker != "" {
		id, err := CanonicalID(o.Marker)
		if err != nil {
			return bad()
		}
		o.Marker = id
	}
	if o.MemberStatus == "" {
		o.MemberStatus = "accepted"
	}
	if !member(o.MemberStatus, "accepted pending rejected all") {
		return bad()
	}
	o.Sort = append([]Sort(nil), o.Sort...)
	if len(o.Sort) == 0 {
		o.Sort = []Sort{{"created_at", "desc"}}
	}
	seen := map[string]bool{}
	for _, s := range o.Sort {
		if !Sortable(s.Key) || (s.Direction != "asc" && s.Direction != "desc") || seen[s.Key] {
			return bad()
		}
		seen[s.Key] = true
	}
	fallback := "desc"
	if len(o.Sort) == 1 {
		fallback = o.Sort[0].Direction
	}
	if o.TieDirection != "" {
		if o.TieDirection != "asc" && o.TieDirection != "desc" {
			return bad()
		}
		fallback = o.TieDirection
	}
	for _, key := range []string{"created_at", "id"} {
		if !seen[key] {
			o.Sort = append(o.Sort, Sort{key, fallback})
		}
	}
	filters := map[string]string{"os_hidden": "false"}
	for key, value := range o.Filters {
		if !utf8.ValidString(key+value) || utf8.RuneCountInString(key) > 255 || len(value) > 65535 {
			return bad()
		}
		switch key {
		case "visibility":
			if !member(value, "private shared public community all") {
				return bad()
			}
		case "status":
			if !member(value, "queued saving active killed deleted pending_delete deactivated uploading importing") {
				return bad()
			}
		case "protected":
			if value != "true" && value != "false" {
				return bad()
			}
		case "os_hidden":
			value = strings.ToLower(value)
			if value != "true" && value != "false" {
				return bad()
			}
		case "disk_format":
			if !member(value, "ami ari aki vhd vhdx vmdk raw qcow2 vdi iso ploop") {
				return bad()
			}
		case "container_format":
			if !member(value, "ami ari aki bare ovf ova docker compressed") {
				return bad()
			}
		case "name", "owner":
			if utf8.RuneCountInString(value) > 255 {
				return bad()
			}
		case "id":
			id, err := CanonicalID(value)
			if err != nil {
				return bad()
			}
			value = id
		case "min_disk", "min_ram":
			n, err := strconv.ParseInt(value, 10, 32)
			if err != nil || n < 0 {
				return bad()
			}
		case "size_min", "size_max", "virtual_size_min", "virtual_size_max":
			return bad()
		default:
			if Reserved(key) || key == "member_status" || key == "changes-since" {
				return bad()
			}
		}
		filters[key] = value
	}
	o.Filters = filters
	if len(filters) > 128 {
		return bad()
	}
	o.Tags = append([]string(nil), o.Tags...)
	for _, tag := range o.Tags {
		if !utf8.ValidString(tag) || utf8.RuneCountInString(tag) > 255 {
			return bad()
		}
	}
	return o, nil
}

// PatchOperation uses one decoded JSON Pointer token. Nested locations/tags,
// test, copy and move are deliberately outside the selected contract.
type PatchOperation struct {
	Op, Key string
	Value   json.RawMessage
}

func Mutable(key string) bool {
	return member(key, "name disk_format container_format visibility min_disk min_ram protected os_hidden tags")
}

// ApplyPatch never aliases the input maps/slices, including on a later error.
// Each operation is validated before proceeding to the next one.
func ApplyPatch(image Image, operations []PatchOperation, now time.Time) (Image, error) {
	if len(operations) > 128 {
		return Image{}, Fail(Invalid, "Too many patch operations.")
	}
	image.Properties = cloneProperties(image.Properties)
	image.Tags = append([]string{}, image.Tags...)
	if image.Name != nil {
		v := *image.Name
		image.Name = &v
	}
	if image.DiskFormat != nil {
		v := *image.DiskFormat
		image.DiskFormat = &v
	}
	if image.ContainerFormat != nil {
		v := *image.ContainerFormat
		image.ContainerFormat = &v
	}
	for _, op := range operations {
		if !member(op.Op, "add replace remove") {
			return Image{}, Fail(Invalid, "Unsupported JSON Patch operation.")
		}
		if op.Key == "locations" { // locations are invisible (show_multiple_locations off)
			verb := map[string]string{"add": "add", "replace": "update", "remove": "remove"}[op.Op]
			return Image{}, Fail(Forbidden, "It's not allowed to "+verb+" locations if locations are invisible.")
		}
		if Reserved(op.Key) && !Mutable(op.Key) {
			return Image{}, Fail(Forbidden, "Attribute is immutable or reserved: "+op.Key)
		}
		if op.Op == "remove" {
			if Mutable(op.Key) {
				return Image{}, Fail(Forbidden, "Base properties may not be removed.")
			}
			if _, exists := image.Properties[op.Key]; !exists {
				return Image{}, Fail(Conflict, "Property does not exist.")
			}
			delete(image.Properties, op.Key)
		} else {
			if len(op.Value) == 0 || !utf8.Valid(op.Value) {
				return Image{}, Fail(Invalid, "Patch value is required and must be UTF-8 JSON.")
			}
			if !Mutable(op.Key) {
				if _, exists := image.Properties[op.Key]; !exists && op.Op == "replace" {
					return Image{}, Fail(Conflict, "Property does not exist.")
				}
				var value *string
				if json.Unmarshal(op.Value, &value) != nil || value == nil {
					return Image{}, Fail(Invalid, "Additional properties must be strings.")
				}
				image.Properties[op.Key] = *value
			} else {
				if (op.Key == "disk_format" || op.Key == "container_format") && image.Status != "queued" {
					return Image{}, Fail(Conflict, "Image formats may only be changed while queued.")
				}
				// Decode nullable strings separately from non-nullable scalar values.
				var err error
				switch op.Key {
				case "name":
					err = json.Unmarshal(op.Value, &image.Name)
				case "disk_format":
					err = json.Unmarshal(op.Value, &image.DiskFormat)
				case "container_format":
					err = json.Unmarshal(op.Value, &image.ContainerFormat)
				default:
					if strings.TrimSpace(string(op.Value)) == "null" {
						return Image{}, Fail(Invalid, "Base property cannot be null.")
					}
					switch op.Key {
					case "visibility":
						err = json.Unmarshal(op.Value, &image.Visibility)
					case "min_disk":
						err = json.Unmarshal(op.Value, &image.MinDisk)
					case "min_ram":
						err = json.Unmarshal(op.Value, &image.MinRAM)
					case "protected":
						err = json.Unmarshal(op.Value, &image.Protected)
					case "os_hidden":
						err = json.Unmarshal(op.Value, &image.Hidden)
					case "tags":
						var tags []*string
						err = json.Unmarshal(op.Value, &tags)
						image.Tags = []string{}
						for _, tag := range tags {
							if tag == nil {
								err = Fail(Invalid, "Tags must be strings.")
								break
							}
							image.Tags = append(image.Tags, *tag)
						}
					}
				}
				if err != nil {
					return Image{}, Fail(Invalid, "Invalid patch value type.")
				}
			}
		}
		validated, err := New(CreateInput{ID: &image.ID, Name: image.Name, DiskFormat: image.DiskFormat, ContainerFormat: image.ContainerFormat,
			Visibility: &image.Visibility, MinDisk: &image.MinDisk, MinRAM: &image.MinRAM, Protected: &image.Protected, Hidden: &image.Hidden,
			Tags: image.Tags, Properties: image.Properties}, image.Owner, image.CreatedAt)
		if err != nil {
			return Image{}, err
		}
		image.Tags, image.Properties = validated.Tags, validated.Properties
	}
	now = now.UTC().Truncate(time.Microsecond)
	if !now.After(image.UpdatedAt) {
		now = image.UpdatedAt.Add(time.Microsecond)
	}
	image.UpdatedAt = now
	return image, nil
}

func cloneProperties(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
