// Package protect is the original's property protections
// (glance/common/property_utils.py): an INI file whose sections are regular
// expressions over property names, each with create, read, update and
// delete permissions as roles (or policy rules), "@" for everyone and "!"
// for no one. The first matching section decides; no match denies. A
// section "os_glance.*" allowing everything is inserted first, as the
// original does.
package protect

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/policy"
)

var Operations = []string{"create", "read", "update", "delete"}

type rule struct {
	section string
	exp     *regexp.Regexp
	perms   map[string][]string
}

// Rules is a loaded property protection file (nil: protections off).
type Rules struct {
	rules    []rule
	policies bool
	enforcer *policy.Enforcer
	// matches caches name -> index of the first matching rule (-1: none).
	// The rules never change after Load, so the answer is the same; the
	// cache is bounded because property names come from users.
	mu      sync.RWMutex
	matches map[string]int
}

const maxCachedNames = 4096

// match is the index of the first rule whose expression matches name.
func (r *Rules) match(name string) int {
	r.mu.RLock()
	n, ok := r.matches[name]
	r.mu.RUnlock()
	if ok {
		return n
	}
	n = -1
	for k := range r.rules {
		if r.rules[k].exp.FindStringIndex(name) != nil {
			n = k
			break
		}
	}
	r.mu.Lock()
	if r.matches == nil {
		r.matches = map[string]int{}
	}
	if len(r.matches) < maxCachedNames {
		r.matches[name] = n
	}
	r.mu.Unlock()
	return n
}

// Load reads the file with format "roles" or "policies".
func Load(path, format string, enforcer *policy.Enforcer) (*Rules, error) {
	format = strings.ToLower(format)
	if format != "roles" && format != "policies" {
		return nil, fmt.Errorf("Invalid value '%s' for 'property_protection_rule_format'. The permitted values are 'roles' and 'policies'", format)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("Couldn't find property protection file %s", path)
	}
	defer f.Close()
	r := &Rules{policies: format == "policies", enforcer: enforcer}
	r.rules = append(r.rules, rule{section: "os_glance.*", exp: regexp.MustCompile("os_glance.*"),
		perms: map[string][]string{"create": {"@"}, "read": {"@"}, "update": {"@"}, "delete": {"@"}}})
	var current *rule
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			name := line[1 : len(line)-1]
			if strings.HasPrefix(name, "os_glance") {
				current = &rule{} // replaced by the inserted section, as the original
				continue
			}
			exp, err := regexp.Compile(name)
			if err != nil {
				return nil, fmt.Errorf("Encountered a malformed property protection rule %s", name)
			}
			r.rules = append(r.rules, rule{section: name, exp: exp, perms: map[string][]string{}})
			current = &r.rules[len(r.rules)-1]
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || current == nil {
				return nil, errors.New("invalid property protection file")
			}
			var list []string
			for _, x := range strings.Split(v, ",") {
				if x = strings.TrimSpace(x); x != "" {
					list = append(list, x)
				}
			}
			current.perms[strings.TrimSpace(k)] = list
		}
	}
	if err = s.Err(); err != nil {
		return nil, err
	}
	for _, x := range r.rules[1:] {
		for _, op := range Operations {
			if _, ok := x.perms[op]; !ok {
				return nil, fmt.Errorf("property protection section %s has no %s permission", x.section, op)
			}
		}
	}
	return r, nil
}

// Allowed is check_property_rules.
func (r *Rules) Allowed(name, op string, roles []string, creds policy.Credentials) bool {
	if r == nil || len(r.rules) == 0 {
		return true
	}
	k := r.match(name)
	if k < 0 {
		return false
	}
	match := &r.rules[k]
	perms := match.perms[op]
	if len(perms) == 0 {
		return false
	}
	for _, p := range perms {
		if p == "!" {
			return false
		}
	}
	for _, p := range perms {
		if p == "@" {
			return true
		}
	}
	if r.policies {
		for _, p := range perms {
			if r.enforcer != nil && r.enforcer.AllowedRule("rule:"+p, creds) {
				return true
			}
		}
		return false
	}
	for _, have := range roles {
		for _, p := range perms {
			if strings.EqualFold(have, p) {
				return true
			}
		}
	}
	return false
}
