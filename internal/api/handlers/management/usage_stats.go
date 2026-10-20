package management

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	log "github.com/sirupsen/logrus"
)

const defaultUsageStatsWindow = 7 * 24 * time.Hour

// GetUsageStats returns aggregated token usage from the persistent usage ledger.
// Query: since (RFC3339 or relative duration such as 24h or 7d, default 7d) and
// until (same formats, default now).
func (h *Handler) GetUsageStats(c *gin.Context) {
	now := time.Now()
	since := now.Add(-defaultUsageStatsWindow)
	if raw := strings.TrimSpace(c.Query("since")); raw != "" {
		parsed, err := parseUsageStatsTime(raw, now)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid since: " + err.Error()})
			return
		}
		since = parsed
	}
	var until time.Time
	if raw := strings.TrimSpace(c.Query("until")); raw != "" {
		parsed, err := parseUsageStatsTime(raw, now)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid until: " + err.Error()})
			return
		}
		until = parsed
	}
	if !until.IsZero() && until.Before(since) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "until must not be before since"})
		return
	}
	c.JSON(http.StatusOK, usagestats.Default().Query(since, until))
}

// DeleteUsageStats clears the persistent usage ledger.
func (h *Handler) DeleteUsageStats(c *gin.Context) {
	cleared, err := usagestats.Default().Clear()
	if err != nil {
		log.WithError(err).Warn("management: failed to persist cleared usage stats")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist cleared usage stats", "cleared": cleared})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "cleared": cleared})
}

// parseUsageStatsTime accepts an RFC3339 timestamp or a relative duration
// ("90m", "24h", "7d") measured back from now.
func parseUsageStatsTime(value string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n < 0 {
			return time.Time{}, fmt.Errorf("%q is not a valid day count", value)
		}
		return now.Add(-time.Duration(n * float64(24*time.Hour))), nil
	}
	if d, err := time.ParseDuration(value); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("%q is not RFC3339 or a duration like 24h or 7d", value)
}
