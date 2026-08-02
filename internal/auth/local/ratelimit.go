package local

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// loginLimiter enforces per-username rate limiting on login attempts.
// Each username is allowed 5 attempts per minute.
type loginLimiter struct {
	mu     sync.Mutex
	limits map[string]*rate.Limiter
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		limits: make(map[string]*rate.Limiter),
	}
}

// Allow reports whether the username is allowed to attempt another login.
func (l *loginLimiter) Allow(username string) bool {
	l.mu.Lock()
	lim, ok := l.limits[username]
	if !ok {
		lim = rate.NewLimiter(rate.Every(time.Minute/5), 5)
		l.limits[username] = lim
	}
	l.mu.Unlock()
	return lim.Allow()
}
