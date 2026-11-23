// Package notify emits the same image notifications as official Glance
// (glance/notifier.py at a3e5f490) in oslo.messaging's v2 wire format.
//
// Official Glance calls its notifier synchronously inside the request; here a
// Notifier never blocks a request: messages are queued and published in order
// by one background worker (see Messaging). The context is {}, as in Glance,
// so no token or identity beyond the payload fields ever leaves the service.
package notify

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

const (
	Info  = "INFO"
	Error = "ERROR"
)

// Notifier accepts one event. Implementations must not block the caller.
type Notifier interface {
	Notify(eventType, priority string, payload any)
}

// Noop is the "noop" driver, and what callers use when nothing is configured.
type Noop struct{}

func (Noop) Notify(string, string, any) {}

// OrNoop returns n, or Noop when n is nil.
func OrNoop(n Notifier) Notifier {
	if n == nil {
		return Noop{}
	}
	return n
}

// Filtered drops events listed in disabled_notifications: an exact event type
// ("image.send") or its group ("image"), as _is_notification_enabled does.
type Filtered struct {
	Next     Notifier
	Disabled map[string]bool
}

func (f Filtered) Notify(eventType, priority string, payload any) {
	group, _, _ := strings.Cut(eventType, ".")
	if f.Disabled[eventType] || f.Disabled[group] {
		return
	}
	f.Next.Notify(eventType, priority, payload)
}

// isotime is glance.common.timeutils.isotime: UTC, seconds, trailing Z.
func isotime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// ImagePayload is format_image_notification: the image without locations.
func ImagePayload(i model.Image) map[string]any {
	properties := map[string]string{}
	for k, v := range i.Properties {
		properties[k] = v
	}
	tags := append([]string{}, i.Tags...)
	return map[string]any{
		"id": i.ID, "name": i.Name, "status": i.Status,
		"created_at": isotime(i.CreatedAt), "updated_at": isotime(i.UpdatedAt),
		"min_disk": i.MinDisk, "min_ram": i.MinRAM, "protected": i.Protected,
		"checksum": i.Checksum, "owner": i.Owner,
		"disk_format": i.DiskFormat, "container_format": i.ContainerFormat,
		"size": i.Size, "virtual_size": i.VirtualSize,
		"is_public": i.Visibility == "public", "visibility": i.Visibility,
		"properties": properties, "tags": tags,
		"deleted": false, "deleted_at": nil,
	}
}

// UploadPayload is the image payload plus _format_import_properties, used by
// image.prepare and image.upload.
func UploadPayload(i model.Image) map[string]any {
	p := ImagePayload(i)
	split := func(key string) []string {
		if v := i.Properties[key]; v != "" {
			return strings.Split(v, ",")
		}
		return []string{}
	}
	p["os_glance_importing_to_stores"] = split("os_glance_importing_to_stores")
	p["os_glance_failed_import"] = split("os_glance_failed_import")
	return p
}

// DeletePayload is image.delete: the removed image with deleted/deleted_at.
func DeletePayload(i model.Image, at time.Time) map[string]any {
	i.Status = "deleted"
	p := ImagePayload(i)
	p["deleted"], p["deleted_at"] = true, isotime(at)
	return p
}

// SendPayload is _format_image_send for image.send.
func SendPayload(imageID, owner, receiverProject, receiverUser string, sent int64) map[string]any {
	return map[string]any{"bytes_sent": sent, "image_id": imageID, "owner_id": owner,
		"receiver_tenant_id": receiverProject, "receiver_user_id": receiverUser}
}

// Envelope is oslo.messaging's v2 notification body for one event.
func Envelope(publisherID, eventType, priority string, payload any, now time.Time) ([]byte, error) {
	unique := uuid.New()
	message, err := json.Marshal(map[string]any{
		"message_id": uuid.NewString(), "publisher_id": publisherID,
		"event_type": eventType, "priority": priority, "payload": payload,
		"timestamp":  now.UTC().Format("2006-01-02 15:04:05.000000"),
		"_unique_id": hex.EncodeToString(unique[:]),
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"oslo.version": "2.0", "oslo.message": string(message)})
}

// MemberPayload is the original's format_image_member_notification
// (image.member.create/update/delete).
func MemberPayload(m model.Member, deleted bool, at time.Time) map[string]any {
	p := map[string]any{"image_id": m.ImageID, "member_id": m.MemberID, "status": m.Status,
		"created_at": isotime(m.CreatedAt), "updated_at": isotime(m.UpdatedAt), "deleted": deleted, "deleted_at": nil}
	if deleted {
		p["deleted_at"] = isotime(at)
	}
	return p
}

// TaskPayload is the original's format_task_notification (task.create,
// task.processing, task.success, task.failure): no input, result or message.
func TaskPayload(t model.Task) map[string]any {
	var expires any
	if t.ExpiresAt != nil {
		expires = isotime(*t.ExpiresAt)
	}
	return map[string]any{"id": t.ID, "type": t.Type, "status": t.Status, "result": nil, "owner": t.Owner, "message": nil,
		"expires_at": expires, "created_at": isotime(t.CreatedAt), "updated_at": isotime(t.UpdatedAt), "deleted": false, "deleted_at": nil}
}

// NamespacePayload is the original's format_metadef_namespace_notification.
func NamespacePayload(n model.Namespace, oldName string) map[string]any {
	var updated any
	if n.UpdatedAt != nil {
		updated = isotime(*n.UpdatedAt)
	}
	if oldName == "" {
		oldName = n.Namespace
	}
	return map[string]any{"namespace": n.Namespace, "namespace_old": oldName, "display_name": n.DisplayName, "protected": n.Protected,
		"visibility": n.Visibility, "owner": n.Owner, "description": n.Description, "created_at": isotime(n.CreatedAt), "updated_at": updated,
		"deleted": false, "deleted_at": nil}
}
