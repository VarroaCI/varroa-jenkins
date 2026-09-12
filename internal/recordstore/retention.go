package recordstore

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// minRetentionDays and maxRetentionDays bound the day count ParseRetention
// accepts. The chart validates the same range before it renders a
// recorder value.
const (
	minRetentionDays = 1
	maxRetentionDays = 3650
)

// ParseRetention parses a retention value of the form "<n>d", with n
// between 1 and 3650. It checks the range before it multiplies, so a
// time.Duration overflow cannot happen.
func ParseRetention(s string) (time.Duration, error) {
	days, ok := strings.CutSuffix(s, "d")
	if !ok || days == "" {
		return 0, fmt.Errorf("retention value %q must have the form <n>d", s)
	}
	n, err := strconv.Atoi(days)
	if err != nil {
		return 0, fmt.Errorf("retention value %q is not a whole day count: %w", s, err)
	}
	if n < minRetentionDays || n > maxRetentionDays {
		return 0, fmt.Errorf("retention value %q must be between %dd and %dd", s, minRetentionDays, maxRetentionDays)
	}
	return time.Duration(n) * 24 * time.Hour, nil
}
