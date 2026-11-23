package repository

import (
	"context"
	"errors"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

var (
	ErrNotFound  = errors.New("image not found")
	ErrDuplicate = errors.New("image UUID already exists")
	ErrConflict  = errors.New("concurrent metadata transaction conflict")
)

type Repository interface {
	Create(context.Context, model.Image) error
	Get(context.Context, string) (model.Image, error)
	List(context.Context, model.ListOptions) ([]model.Image, error)
	// Update and Delete invoke the business callback while holding a row lock.
	// A callback error must cause rollback without any persisted changes.
	Update(context.Context, string, func(model.Image) (model.Image, error)) (model.Image, error)
	Delete(context.Context, string, time.Time, func(model.Image) error) error
}

var ErrMemberNotFound = errors.New("image member not found")
var ErrMemberDuplicate = errors.New("image member already exists")

// Members is the original's image_members table. Writes run under the image
// row lock (as catalog PATCH/DELETE) and call check with the current image
// before changing anything; a check error rolls back.
type Members interface {
	ListMembers(ctx context.Context, imageID string) ([]model.Member, error)
	// GetMember returns ErrMemberNotFound when the project is not a member.
	GetMember(ctx context.Context, imageID, memberID string) (model.Member, error)
	CreateMember(ctx context.Context, imageID, memberID string, now time.Time, check func(image model.Image, members int) error) (model.Member, error)
	UpdateMember(ctx context.Context, imageID, memberID, status string, now time.Time, check func(model.Image, model.Member) error) (model.Member, error)
	DeleteMember(ctx context.Context, imageID, memberID string, now time.Time, check func(model.Image, model.Member) error) (model.Member, error)
}

var ErrTaskNotFound = errors.New("task not found")

// Tasks is the original's tasks/task_info tables and the import lock it
// keeps on the image (property os_glance_import_task).
type Tasks interface {
	// BeginImport locks the image, runs check, moves it from one of from to
	// "importing", sets the lock property to the task and inserts the task.
	BeginImport(ctx context.Context, task model.Task, from []string, check func(model.Image) error) error
	// ReleaseImport, if the image still holds the task's lock: moves it from
	// "importing" to status and clears the lock (keeps it when keep). It
	// reports whether the image held the lock.
	ReleaseImport(ctx context.Context, imageID, taskID, status string, keep bool, now time.Time) (bool, error)
	// SetStaged moves a queued image to uploading (staged) or back.
	SetStaged(ctx context.Context, imageID string, from, to string, now time.Time, check func(model.Image) error) error
	StagedSize(ctx context.Context, imageID string, size int64) error
	UpdateTask(ctx context.Context, task model.Task) error
	GetTask(ctx context.Context, id string) (model.Task, error)
	ListTasks(ctx context.Context, o model.TaskListOptions) ([]model.Task, error)
	ImageTasks(ctx context.Context, imageID string) ([]model.Task, error)
	// OpenTasks lists api_image_import tasks still pending or processing.
	OpenTasks(ctx context.Context) ([]model.Task, error)
}

var (
	ErrMetadefNotFound  = errors.New("metadata definition not found")
	ErrMetadefDuplicate = errors.New("metadata definition already exists")
)

// Metadefs is the original's metadef_* tables.
type Metadefs interface {
	ListNamespaces(ctx context.Context, o model.NamespaceListOptions) ([]model.Namespace, error)
	GetNamespace(ctx context.Context, name string) (model.Namespace, error)
	Associations(ctx context.Context, namespaceID int64) ([]model.ResourceTypeAssociation, error)
	CreateNamespace(ctx context.Context, n model.Namespace, props []model.MetadefProperty, objects []model.MetadefObject, tags []model.MetadefTag, assocs []model.ResourceTypeAssociation) (model.Namespace, error)
	UpdateNamespace(ctx context.Context, id int64, n model.Namespace) error
	DeleteNamespace(ctx context.Context, id int64) error

	Objects(ctx context.Context, namespaceID int64) ([]model.MetadefObject, error)
	Object(ctx context.Context, namespaceID int64, name string) (model.MetadefObject, error)
	CreateObject(ctx context.Context, namespaceID int64, o model.MetadefObject) error
	UpdateObject(ctx context.Context, namespaceID int64, name string, o model.MetadefObject) error
	DeleteObject(ctx context.Context, namespaceID int64, name string) error
	DeleteObjects(ctx context.Context, namespaceID int64) error

	Properties(ctx context.Context, namespaceID int64) ([]model.MetadefProperty, error)
	Property(ctx context.Context, namespaceID int64, name string) (model.MetadefProperty, error)
	CreateProperty(ctx context.Context, namespaceID int64, p model.MetadefProperty) error
	UpdateProperty(ctx context.Context, namespaceID int64, name string, p model.MetadefProperty) error
	DeleteProperty(ctx context.Context, namespaceID int64, name string) error
	DeleteProperties(ctx context.Context, namespaceID int64) error

	Tags(ctx context.Context, namespaceID int64) ([]model.MetadefTag, error)
	Tag(ctx context.Context, namespaceID int64, name string) (model.MetadefTag, error)
	CreateTags(ctx context.Context, namespaceID int64, tags []model.MetadefTag, replace bool) error
	UpdateTag(ctx context.Context, namespaceID int64, name string, t model.MetadefTag) error
	DeleteTag(ctx context.Context, namespaceID int64, name string) error
	DeleteTags(ctx context.Context, namespaceID int64) error

	ResourceTypes(ctx context.Context) ([]model.ResourceType, error)
	Associate(ctx context.Context, namespaceID int64, a model.ResourceTypeAssociation) error
	Dissociate(ctx context.Context, namespaceID int64, name string) error
}
