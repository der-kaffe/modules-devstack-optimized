// Package mysql implements Glance Go's repository over the ORIGINAL Glance
// catalog (database glance) plus its own transfer bookkeeping in
// glance_go_shared, on the same MySQL server and with the original's account.
package mysql

import (
	"database/sql"
	"errors"
	"net"
	"regexp"
	"strings"
	"time"

	driver "github.com/go-sql-driver/mysql"
)

// AllowedDatabase: the DSN names Glance Go's bookkeeping database.
func AllowedDatabase(name string) bool { return Shared(name) }

// SharedDatabase holds only Glance Go's own transfer bookkeeping; the catalog
// is the official Glance database (OfficialDatabase).
const (
	SharedDatabase   = "glance_go_shared"
	OfficialDatabase = "glance"
)

var sharedTestDatabase = regexp.MustCompile(`^glance_go_shared_test_[a-z0-9_]+$`)

// Shared reports whether a Glance Go database pairs with the official catalog.
func Shared(name string) bool { return name == SharedDatabase || sharedTestDatabase.MatchString(name) }

// parseDSN accepts the original Glance's own account (DevStack's root, read
// from its glance-api.conf) on the bookkeeping database: the same account the
// original uses, never a new privileged one.
func parseDSN(value string) (*driver.Config, error) {
	return parseDSNFor(value, AllowedDatabase, "MySQL database must be glance_go_shared or glance_go_shared_test_<suffix>")
}

func parseDSNFor(value string, allowed func(string) bool, refusal string) (*driver.Config, error) {
	c, err := driver.ParseDSN(value)
	if err != nil {
		return nil, errors.New("invalid MySQL DSN")
	}
	if !allowed(c.DBName) {
		return nil, errors.New(refusal)
	}
	if c.User == "" {
		return nil, errors.New("a database account is required")
	}
	if c.MultiStatements || c.AllowAllFiles || c.AllowCleartextPasswords || c.AllowOldPasswords || c.AllowFallbackToPlaintext || c.TLSConfig == "skip-verify" || c.TLSConfig == "preferred" {
		return nil, errors.New("unsafe MySQL DSN options")
	}
	// Plaintext is allowed only on a local socket or a literal loopback address.
	// Remote connections require certificate-verified TLS; custom registrations
	// are deliberately unsupported because their verification cannot be inferred.
	local := c.Net == "unix"
	if c.Net == "tcp" {
		host, _, err := net.SplitHostPort(c.Addr)
		local = err == nil && net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
	}
	if c.TLSConfig != "" && c.TLSConfig != "false" && c.TLSConfig != "true" {
		return nil, errors.New("only verified MySQL TLS is supported")
	}
	if !local && c.TLSConfig != "true" {
		return nil, errors.New("remote MySQL connections require verified TLS")
	}
	c.ParseTime = true
	c.Loc = time.UTC
	c.InterpolateParams = false
	// Rows affected = rows matched, as SQLAlchemy's MySQL dialects connect
	// (CLIENT_FOUND_ROWS). The original's timestamps are whole seconds, so a
	// metadata-only PATCH within the same second leaves the images row
	// unchanged; counting changed rows would report it as a conflict.
	c.ClientFoundRows = true
	// Like pymysql, which the original uses: 10 s to connect and no read or
	// write timeout. A query waits for MySQL (e.g. a commit stalled by disk
	// writeback) within the deadline of the request that issued it.
	if c.Timeout == 0 {
		c.Timeout = 10 * time.Second
	}
	if c.Params == nil {
		c.Params = map[string]string{}
	}
	for key, value := range c.Params {
		if strings.EqualFold(key, "autocommit") {
			if key != "autocommit" || (value != "1" && !strings.EqualFold(value, "true")) {
				return nil, errors.New("MySQL autocommit must be enabled")
			}
		}
	}
	c.Params["autocommit"] = "1"
	// TIME_TRUNCATE_FRACTIONAL: the official catalog stores whole seconds;
	// truncate like Python's oslo.db instead of rounding up.
	c.Params["sql_mode"] = "'STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION,TIME_TRUNCATE_FRACTIONAL'"
	return c, nil
}

// Open constructs a pool; it does not connect, create schemas or run migrations.
func Open(dsn string) (*sql.DB, error) {
	c, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	return openConfig(c)
}

// OpenCatalog opens the catalog database itself for the command-line tools
// (glance-manage, cache tools, scrubber), with the same connection rules.
func OpenCatalog(dsn string) (*sql.DB, error) {
	c, err := parseDSNFor(dsn, func(name string) bool { return catalogName.MatchString(name) }, "invalid catalog database name")
	if err != nil {
		return nil, err
	}
	return openConfig(c)
}

var catalogName = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

func openConfig(c *driver.Config) (*sql.DB, error) {
	connector, err := driver.NewConnector(c)
	if err != nil {
		return nil, errors.New("invalid MySQL connector configuration")
	}
	db := sql.OpenDB(connector)
	// Official Glance uses oslo.db's SQLAlchemy pool: 5 kept + 50 overflow.
	// Same ceiling; more kept idle so bursts do not reconnect (was 4 / 2).
	db.SetMaxOpenConns(PoolMaxOpen)
	db.SetMaxIdleConns(PoolMaxIdle)
	db.SetConnMaxLifetime(3 * time.Minute)
	return db, nil
}

// PoolMaxOpen and PoolMaxIdle are the SQL pool limits set by Open.
const (
	PoolMaxOpen = 55
	PoolMaxIdle = 16
)

// OfficialCatalogName matches the official database the bookkeeping pairs with.
func OfficialCatalogName(name string) bool {
	return name == OfficialDatabase || officialTestDatabase.MatchString(name)
}

var officialTestDatabase = regexp.MustCompile(`^glance_official_test_[a-z0-9_]+$`)

func ValidateTestDSN(value string) error {
	c, err := parseDSN(value)
	if err != nil {
		return err
	}
	if !sharedTestDatabase.MatchString(c.DBName) {
		return errors.New("integration tests require glance_go_shared_test_<suffix>")
	}
	if strings.Contains(c.Addr, "192.168.122.10") || strings.Contains(c.Addr, "10.0.2.15") {
		return errors.New("integration tests must not target the existing thesis VM")
	}
	return nil
}
