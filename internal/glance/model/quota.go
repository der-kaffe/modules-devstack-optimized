package model

// Usage is what a project consumes of the original's four Keystone limits
// (glance/quota/keystone.py); sizes are bytes here and MiB in the limits.
type Usage struct {
	// SizeTotal: size times active locations of every image not killed or
	// deleted (user_get_storage_usage).
	SizeTotal int64
	// StageTotal: bytes in the staging area: images uploading or importing,
	// and images being copied to another store (user_get_staging_usage).
	StageTotal int64
	// CountTotal: images not deleted (user_get_image_count).
	CountTotal int64
	// CountUploading: images saving, uploading or importing, plus copies in
	// progress (user_get_uploading_count).
	CountUploading int64
}
