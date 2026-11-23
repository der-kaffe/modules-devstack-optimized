package model

import "time"

// CachedImage is one row of the original's cached_images (centralized_db).
type CachedImage struct {
	ImageID                    string
	LastAccessed, LastModified time.Time
	Size                       int64
	Hits                       int
	Checksum                   *string
}
