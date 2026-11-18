package auth

import (
	"context"
	"testing"
	"time"
)

// Local Keystone fixture over loopback HTTP; a real Keystone is slower, so the
// gap in the laboratory is larger. Not a campaign result.
func benchmarkAuthenticate(b *testing.B, ttl time.Duration) {
	k, _ := compositeValidator(b, map[string]compositeToken{
		"user": {user: "u", project: "p", roles: []string{"reader"}, expires: time.Now().Add(time.Hour)},
	})
	WithTokenCache(ttl, 100)(k)
	for b.Loop() {
		if _, err := k.Authenticate(context.Background(), "user"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAuthenticateValidateEveryCall(b *testing.B) { benchmarkAuthenticate(b, 0) }
func BenchmarkAuthenticateTokenCache(b *testing.B)        { benchmarkAuthenticate(b, 5*time.Minute) }
