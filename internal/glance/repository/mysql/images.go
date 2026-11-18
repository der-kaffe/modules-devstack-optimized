package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	driver "github.com/go-sql-driver/mysql"
)

type Repository struct {
	db      *sql.DB
	timeout time.Duration
	// stmts holds server-side prepared statements for hot static reads,
	// created once by PrepareStatements (outside any transaction) and then
	// only read. nil means every query is prepared and executed per call.
	stmts map[string]*sql.Stmt
}

var _ repository.Repository = (*Repository)(nil)

func New(db *sql.DB, timeout time.Duration) *Repository { return &Repository{db: db, timeout: timeout} }

func (r *Repository) Create(ctx context.Context, image model.Image) error {
	if image.Status != "queued" {
		return errors.New("only queued metadata can be created")
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin metadata transaction: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO images
		(id, owner, name, status, visibility, disk_format, container_format, min_disk, min_ram, protected, os_hidden, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		image.ID, image.Owner, image.Name, image.Status, image.Visibility, image.DiskFormat, image.ContainerFormat,
		image.MinDisk, image.MinRAM, image.Protected, image.Hidden, image.CreatedAt.UTC(), image.UpdatedAt.UTC())
	if err != nil {
		var my *driver.MySQLError
		if errors.As(err, &my) && my.Number == 1062 {
			return repository.ErrDuplicate
		}
		return fmt.Errorf("insert image: %w", err)
	}
	if err = insertChildren(ctx, tx, image.ID, image.Properties, image.Tags); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit metadata transaction: %w", err)
	}
	return nil
}

func (r *Repository) Get(ctx context.Context, id string) (model.Image, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return model.Image{}, fmt.Errorf("begin metadata read: %w", err)
	}
	defer tx.Rollback()
	image, err := r.readImage(ctx, tx, id, false)
	if err != nil {
		return model.Image{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Image{}, fmt.Errorf("complete metadata read: %w", err)
	}
	return image, nil
}
