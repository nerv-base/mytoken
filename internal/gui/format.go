package gui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

// fmtUSD formats an API-equivalent cost: cents below $1000, whole dollars
// with separators above, compact past a million.
func fmtUSD(v float64) string {
	v *= curRate
	sym := curSym
	switch {
	case v == 0:
		return sym + "0"
	case v < 0.01:
		return "<" + sym + "0.01"
	case v < 1000:
		return sym + strconv.FormatFloat(v, 'f', 2, 64)
	case v < 1e6:
		return sym + group(int64(math.Round(v)))
	}
	return sym + strconv.FormatFloat(v/1e6, 'f', 2, 64) + "M"
}

// fmtUSDShort is fmtUSD without cents from 100 up, for axes and tight spots.
func fmtUSDShort(v float64) string {
	w := v * curRate
	switch {
	case w == 0:
		return curSym + "0"
	case w < 100:
		return fmtUSD(v)
	case w < 10000:
		return curSym + group(int64(math.Round(w)))
	case w < 1e6:
		return curSym + strconv.FormatFloat(w/1e3, 'f', 1, 64) + "k"
	}
	return curSym + strconv.FormatFloat(w/1e6, 'f', 1, 64) + "M"
}

// fmtTokens is a compact token count: 950, 12.3K, 4.56M, 1.2B.
func fmtTokens(n int64) string {
	f := float64(n)
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 1e6:
		return trimZero(strconv.FormatFloat(f/1e3, 'f', 1, 64)) + "K"
	case n < 1e9:
		return trimZero(strconv.FormatFloat(f/1e6, 'f', 2, 64)) + "M"
	}
	return trimZero(strconv.FormatFloat(f/1e9, 'f', 2, 64)) + "B"
}

func trimZero(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// fmtInt groups an integer: 12,345.
func fmtInt(n int64) string { return group(n) }

func group(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fmtPct formats a fraction as a percentage.
func fmtPct(f float64) string {
	switch {
	case f <= 0:
		return "0%"
	case f < 0.001:
		return "<0.1%"
	case f < 0.1:
		return strconv.FormatFloat(f*100, 'f', 1, 64) + "%"
	}
	return strconv.FormatFloat(f*100, 'f', 0, 64) + "%"
}

// fmtAgo is a short relative time.
func fmtAgo(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case t.IsZero():
		return "—"
	case d < time.Minute:
		return tr("justNow")
	case d < time.Hour:
		return fmt.Sprintf(tr("minAgo"), int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf(tr("hourAgo"), int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf(tr("dayAgo"), int(d.Hours()/24))
	}
	return fmtDate(t)
}

// fmtIn is a short "in 2h 10m" for a reset time.
func fmtIn(now, t time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return tr("resetSoon")
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf(tr("resetsInM"), int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf(tr("resetsInHM"), int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf(tr("resetsInD"), int(d.Hours()/24))
}

// fmtDuration is a compact duration: 45s, 12m, 3h 5m.
func fmtDuration(d time.Duration) string {
	zh := lang == "zh"
	switch {
	case d < time.Minute && zh:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour && zh:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case zh:
		return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

func fmtDate(t time.Time) string {
	if lang == "zh" {
		return fmt.Sprintf("%d月%d日", t.Month(), t.Day())
	}
	return t.Format("Jan 2")
}

func fmtDateYear(t time.Time) string {
	if lang == "zh" {
		return fmt.Sprintf("%d年%d月%d日", t.Year(), t.Month(), t.Day())
	}
	return t.Format("Jan 2, 2006")
}

func fmtDateTime(t time.Time) string {
	if lang == "zh" {
		return fmt.Sprintf("%d月%d日 %s", t.Month(), t.Day(), t.Format("15:04"))
	}
	return t.Format("Jan 2 15:04")
}

func fmtMonth(t time.Time) string {
	if lang == "zh" {
		return fmt.Sprintf("%d月", t.Month())
	}
	return t.Format("January")
}

func fmtMonthShort(t time.Time) string {
	if lang == "zh" {
		return fmt.Sprintf("%d月", t.Month())
	}
	return t.Format("Jan")
}

func weekday(t time.Time) string {
	if lang == "zh" {
		return [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[t.Weekday()]
	}
	return t.Format("Mon")
}

func fmtF(v float64) string { return fmt.Sprintf("%.4f", v) }

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// shown is the headline token count: everything except cache reads, which
// would otherwise dwarf the work actually done (PLAN-0.5: 主数字不含缓存读取).
func shown(t model.Tokens) int64 { return t.Total() - t.CacheRead }
