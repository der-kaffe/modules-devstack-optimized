package model

import (
	"strings"
	"time"
)

type DataPhase string

const (
	DataReserved  DataPhase = "reserved"
	DataStaged    DataPhase = "staged"
	DataPublished DataPhase = "published"
	DataCommitted DataPhase = "committed"
	DataCleanup   DataPhase = "cleanup"
	DataCleaned   DataPhase = "cleaned"
)

// DataContent is a receipt for measured bytes, not proof of filesystem durability.
// The service must confirm publication before asking the repository to commit.
type DataContent struct {
	Size        int64
	VirtualSize *int64
	Checksum    string
	SHA512      string
}

type DataOperation struct {
	ImageID, AttemptID              string
	Phase                           DataPhase
	ExpectedSize, Size, VirtualSize *int64
	Checksum, SHA512                *string
	CleanupTarget, LastErrorCode    *string
	CreatedAt, UpdatedAt            time.Time
}

// DataState is read in a fresh, locking transaction for outcome resolution.
// It includes tombstones and is internal, never an HTTP metadata representation.
type DataState struct {
	ImageID, Status                    string
	Deleted                            bool
	Size, VirtualSize                  *int64
	Checksum, HashAlgorithm, HashValue *string
	UpdatedAt                          time.Time
	Operation                          *DataOperation
	// Backend is the store of the image's first active location in a
	// multi-store deployment ("" for the default store).
	Backend string
}

func lowerHex(s string, length int) bool {
	if len(s) != length {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func (c DataContent) Validate(expected *int64) error {
	if c.Size < 0 || (c.VirtualSize != nil && *c.VirtualSize < 0) || !lowerHex(c.Checksum, 32) || !lowerHex(c.SHA512, 128) {
		return Fail(Invalid, "Invalid measured content receipt.")
	}
	if expected != nil && (*expected < 0 || *expected != c.Size) {
		return Fail(Invalid, "Measured size does not match declared size.")
	}
	return nil
}

func DataErrorCode(code *string) bool {
	return code == nil || member(*code, "interrupted invalid_size size_limit store_full store_io integrity cancelled recovery deleted terminal")
}

func (o DataOperation) Validate() error {
	bad := func() error { return Fail(Invalid, "Incompatible image data operation.") }
	for _, id := range []string{o.ImageID, o.AttemptID} {
		canonical, err := CanonicalID(id)
		if err != nil || canonical != id {
			return bad()
		}
	}
	if !member(string(o.Phase), "reserved staged published committed cleanup cleaned") || !DataErrorCode(o.LastErrorCode) {
		return bad()
	}
	for _, n := range []*int64{o.ExpectedSize, o.Size, o.VirtualSize} {
		if n != nil && *n < 0 {
			return bad()
		}
	}
	if o.Checksum != nil && !lowerHex(*o.Checksum, 32) || o.SHA512 != nil && !lowerHex(*o.SHA512, 128) {
		return bad()
	}
	if o.CleanupTarget != nil && !member(*o.CleanupTarget, "queued deleted killed") {
		return bad()
	}
	if (o.Phase == DataCleanup || o.Phase == DataCleaned) && o.CleanupTarget == nil {
		return bad()
	}
	if o.Phase == DataStaged || o.Phase == DataPublished || o.Phase == DataCommitted {
		if o.Size == nil || o.Checksum == nil || o.SHA512 == nil {
			return bad()
		}
		return (DataContent{Size: *o.Size, VirtualSize: o.VirtualSize, Checksum: *o.Checksum, SHA512: *o.SHA512}).Validate(o.ExpectedSize)
	}
	return nil
}
