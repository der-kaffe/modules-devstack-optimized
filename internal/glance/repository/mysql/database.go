// Package mysql implements the isolated Glance Go metadata repository.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"regexp"
	"strings"
	"time"

	driver "github.com/go-sql-driver/mysql"
)

var testDatabase = regexp.MustCompile(`^glance_go_test_[a-z0-9_]+$`)

func AllowedDatabase(name string) bool { return name == "glance_go" || testDatabase.MatchString(name) }

func parseDSN(value string) (*driver.Config, error) {
	c, err := driver.ParseDSN(value)
	if err != nil {
		return nil, errors.New("invalid MySQL DSN")
	}
	if !AllowedDatabase(c.DBName) {
		return nil, errors.New("MySQL database must be glance_go or glance_go_test_<suffix>")
	}
	if c.User == "" {
		return nil, errors.New("a dedicated database account is required")
	}
	switch strings.ToLower(c.User) {
	case "root", "admin", "stack", "glance", "nova", "placement", "keystone":
		return nil, errors.New("original service and administrative database accounts are prohibited")
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
	if c.Timeout == 0 {
		c.Timeout = 5 * time.Second
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 5 * time.Second
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = 5 * time.Second
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
	c.Params["sql_mode"] = "'STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION'"
	return c, nil
}

// Open constructs a pool; it does not connect, create schemas or run migrations.
func Open(dsn string) (*sql.DB, error) {
	c, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	connector, err := driver.NewConnector(c)
	if err != nil {
		return nil, errors.New("invalid MySQL connector configuration")
	}
	db := sql.OpenDB(connector)
	// Official Glance uses oslo.db's SQLAlchemy pool: 5 kept + 50 overflow.
	// Same ceiling; more kept idle so bursts do not reconnect (was 4 / 2).
	db.SetMaxOpenConns(55)
	db.SetMaxIdleConns(16)
	db.SetConnMaxLifetime(3 * time.Minute)
	return db, nil
}

// CheckAccount refuses grants outside the dedicated schema, global privileges,
// delegated roles and grant options. No grant text is returned or logged.
func CheckAccount(ctx context.Context, db *sql.DB, mode string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return errors.New("cannot acquire account inspection connection")
	}
	defer conn.Close()
	return checkAccount(ctx, conn, mode)
}

func checkAccount(ctx context.Context, conn *sql.Conn, mode string) error {
	if mode != "serve" && mode != "migrate" {
		return errors.New("invalid database account mode")
	}
	var database string
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); err != nil {
		return errors.New("cannot verify database scope")
	}
	if !AllowedDatabase(database) {
		return errors.New("database scope is prohibited")
	}
	var literalWildcards bool
	if conn.QueryRowContext(ctx, "SELECT @@GLOBAL.partial_revokes").Scan(&literalWildcards) != nil {
		return errors.New("cannot verify literal database grant semantics")
	}
	// SHOW GRANTS without FOR includes mandatory role assignments, but does
	// not expand their privileges. This delivery supports direct grants only:
	// reject all mandatory/active/granted roles, including inactive defaults.
	// Do not request administrative access to mysql.* to inspect role graphs.
	var mandatory, active string
	if conn.QueryRowContext(ctx, "SELECT @@GLOBAL.mandatory_roles, CURRENT_ROLE()").Scan(&mandatory, &active) != nil {
		return errors.New("cannot verify database roles")
	}
	if strings.TrimSpace(mandatory) != "" || active != "NONE" {
		return errors.New("database roles are unsupported; use direct schema grants without mandatory or active roles")
	}
	rows, err := conn.QueryContext(ctx, "SHOW GRANTS")
	if err != nil {
		return errors.New("cannot verify database grants")
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var grant string
		if rows.Scan(&grant) != nil {
			return errors.New("cannot read database grants")
		}
		if !allowedGrant(grant, database, mode, literalWildcards) {
			return errors.New("database account has privileges outside the selected mode or schema")
		}
		count++
	}
	if rows.Err() != nil || count == 0 {
		return errors.New("cannot verify database grants")
	}
	return nil
}

func allowedGrant(grant, database, mode string, literalWildcards bool) bool {
	upper := strings.ToUpper(grant)
	if strings.Contains(upper, "WITH GRANT OPTION") {
		return false
	}
	if strings.HasPrefix(upper, "GRANT USAGE ON *.* TO ") {
		return true
	}
	// With partial_revokes disabled, underscores in schema-level grants are
	// wildcards even in backticks. Only the escaped literal scope is accepted.
	scope := database
	if !literalWildcards {
		scope = strings.ReplaceAll(scope, "_", `\_`)
	}
	marker := " ON `" + scope + "`.* TO "
	parts := strings.SplitN(grant, marker, 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "GRANT ") {
		return false
	}
	permissions := strings.TrimPrefix(parts[0], "GRANT ")
	allowed := map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true}
	if mode == "migrate" {
		for _, p := range []string{"CREATE", "ALTER", "INDEX", "REFERENCES"} {
			allowed[p] = true
		}
	}
	for _, p := range strings.Split(permissions, ",") {
		if !allowed[strings.TrimSpace(p)] {
			return false
		}
	}
	return true
}

// ValidateTestDSN requires a dedicated test schema even with an explicit opt-in.
func ValidateTestDSN(value string) error {
	c, err := parseDSN(value)
	if err != nil {
		return err
	}
	if !testDatabase.MatchString(c.DBName) {
		return errors.New("integration tests require glance_go_test_<suffix>")
	}
	if strings.Contains(c.Addr, "192.168.122.10") || strings.Contains(c.Addr, "10.0.2.15") {
		return errors.New("integration tests must not target the existing thesis VM")
	}
	return nil
}
