package schedexpr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// searchYears bounds Next. Ten years covers every expression that fires at
// all in practice, including a 29 February that skips a non-leap century
// year, while keeping the worst case to a few thousand day steps.
const searchYears = 10

// cronSchedule is a parsed cron expression. Each set is indexed by the
// field's value (dow by time.Weekday); year is nil when any year matches.
type cronSchedule struct {
	minute, hour, dom, month []bool
	dow                      [7]bool
	year                     []bool
	// orDays makes a day match when either day-of-month or day-of-week
	// matches (classic cron when both fields are restricted); otherwise
	// both must match.
	orDays bool
}

// dialect captures how the cron syntaxes differ: field count and the
// day-of-month/day-of-week rules.
type dialect struct {
	name      string
	fields    int
	parseDays func(c *cronSchedule, dom, dow string) error
}

var (
	eventBridgeDialect = dialect{name: "EventBridge cron", fields: 6, parseDays: parseEventBridgeDays}
	unixDialect        = dialect{name: "cron", fields: 5, parseDays: parseUnixDays}
)

// field describes one cron field's value range and optional names.
type field struct {
	lo, hi int
	names  map[string]int
}

var (
	minuteField = field{lo: 0, hi: 59}
	hourField   = field{lo: 0, hi: 23}
	domField    = field{lo: 1, hi: 31}
	monthField  = field{lo: 1, hi: 12, names: map[string]int{
		"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
		"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
	}}
	yearField = field{lo: 1970, hi: 2199}
	// EventBridge numbers days of the week 1-7 from Sunday.
	eventBridgeDowField = field{lo: 1, hi: 7, names: map[string]int{
		"SUN": 1, "MON": 2, "TUE": 3, "WED": 4, "THU": 5, "FRI": 6, "SAT": 7,
	}}
	// Classic cron numbers them 0-6 from Sunday and also accepts 7 for Sunday.
	unixDowField = field{lo: 0, hi: 7, names: map[string]int{
		"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
	}}
)

// parseCron parses the space-separated fields of a cron expression.
func parseCron(fields []string, d dialect) (Schedule, error) {
	if len(fields) != d.fields {
		return nil, fmt.Errorf("%s: want %d fields, got %d", d.name, d.fields, len(fields))
	}
	c := &cronSchedule{}
	var err error
	if c.minute, err = parseField(fields[0], minuteField); err != nil {
		return nil, fmt.Errorf("%s minutes: %w", d.name, err)
	}
	if c.hour, err = parseField(fields[1], hourField); err != nil {
		return nil, fmt.Errorf("%s hours: %w", d.name, err)
	}
	if c.month, err = parseField(fields[3], monthField); err != nil {
		return nil, fmt.Errorf("%s month: %w", d.name, err)
	}
	if err = d.parseDays(c, fields[2], fields[4]); err != nil {
		return nil, fmt.Errorf("%s: %w", d.name, err)
	}
	if d.fields == 6 && fields[5] != "*" {
		if c.year, err = parseField(fields[5], yearField); err != nil {
			return nil, fmt.Errorf("%s year: %w", d.name, err)
		}
	}
	return c, nil
}

// parseEventBridgeDays applies EventBridge's rule that exactly one of
// day-of-month and day-of-week is "?" (no constraint); the other field alone
// decides which days match.
func parseEventBridgeDays(c *cronSchedule, dom, dow string) error {
	if (dom == "?") == (dow == "?") {
		return errors.New("exactly one of day-of-month and day-of-week must be ?")
	}
	if dom == "?" {
		dom = "*"
	}
	var err error
	if c.dom, err = parseField(dom, domField); err != nil {
		return fmt.Errorf("day-of-month: %w", err)
	}
	if dow == "?" {
		dow = "*"
	}
	days, err := parseField(dow, eventBridgeDowField)
	if err != nil {
		return fmt.Errorf("day-of-week: %w", err)
	}
	for v := 1; v <= 7; v++ {
		c.dow[v-1] = days[v]
	}
	return nil
}

// parseUnixDays applies classic cron's rule: when both day fields are
// restricted a day matches either; when either is "*" (or "?") both must
// match. "*/n" is treated as restricted by some implementations and as "*"
// by others, so combining it with a restricted other field is rejected
// rather than guessed.
func parseUnixDays(c *cronSchedule, dom, dow string) error {
	domStar, dowStar := isStar(dom), isStar(dow)
	if (strings.HasPrefix(dom, "*/") && !dowStar) || (strings.HasPrefix(dow, "*/") && !domStar) {
		return errors.New("a */n day field combined with a restricted other day field is ambiguous")
	}
	var err error
	if c.dom, err = parseField(starOf(dom), domField); err != nil {
		return fmt.Errorf("day-of-month: %w", err)
	}
	days, err := parseField(starOf(dow), unixDowField)
	if err != nil {
		return fmt.Errorf("day-of-week: %w", err)
	}
	for v := 0; v <= 7; v++ {
		c.dow[v%7] = c.dow[v%7] || days[v]
	}
	c.orDays = !domStar && !dowStar
	return nil
}

func isStar(s string) bool { return s == "*" || s == "?" }

// starOf maps "?" (accepted as "*" by Kubernetes CronJobs) to "*".
func starOf(s string) string {
	if s == "?" {
		return "*"
	}
	return s
}

// parseField parses a comma-separated list of values, ranges and steps
// into a set indexed by value.
func parseField(spec string, f field) ([]bool, error) {
	set := make([]bool, f.hi+1)
	for _, part := range strings.Split(spec, ",") {
		start, end, step, err := parseRange(part, f)
		if err != nil {
			return nil, err
		}
		for v := start; v <= end; v += step {
			set[v] = true
		}
	}
	return set, nil
}

// parseRange parses one list element: "*", "v", "a-b", each optionally
// followed by "/step". "v/step" runs from v to the field maximum.
func parseRange(part string, f field) (start, end, step int, err error) {
	rng, stepText, hasStep := strings.Cut(part, "/")
	step = 1
	if hasStep {
		if step, err = number(stepText); err != nil || step < 1 {
			return 0, 0, 0, fmt.Errorf("invalid step in %q", part)
		}
	}
	if rng == "*" {
		return f.lo, f.hi, step, nil
	}
	loText, hiText, isRange := strings.Cut(rng, "-")
	if start, err = f.value(loText); err != nil {
		return 0, 0, 0, err
	}
	end = start
	if isRange {
		end, err = f.value(hiText)
	} else if hasStep {
		end = f.hi
	}
	if err != nil {
		return 0, 0, 0, err
	}
	if end < start {
		return 0, 0, 0, fmt.Errorf("range %q runs backwards", rng)
	}
	return start, end, step, nil
}

// value parses a single field value: a name or an in-range number.
func (f field) value(s string) (int, error) {
	if v, ok := f.names[strings.ToUpper(s)]; ok {
		return v, nil
	}
	v, err := number(s)
	if err != nil || v < f.lo || v > f.hi {
		return 0, fmt.Errorf("value %q outside %d-%d", s, f.lo, f.hi)
	}
	return v, nil
}

// number parses an unsigned decimal; unlike strconv.Atoi it rejects signs,
// so "+5" or "-5" cannot slip through as a value.
func number(s string) (int, error) {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return strconv.Atoi(s)
}

// Next implements Schedule. It walks forward a day, an hour or a minute at a
// time, whichever is the smallest step that can still reach a match.
func (c *cronSchedule) Next(after time.Time) (time.Time, bool) {
	t := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(searchYears, 0, 0)
	for t.Before(limit) {
		switch {
		case !c.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
		case !c.hour[t.Hour()]:
			t = t.Truncate(time.Hour).Add(time.Hour)
		case !c.minute[t.Minute()]:
			t = t.Add(time.Minute)
		default:
			return t, true
		}
	}
	return time.Time{}, false
}

func (c *cronSchedule) dayMatches(t time.Time) bool {
	if !c.yearMatches(t.Year()) {
		return false
	}
	if !c.month[t.Month()] {
		return false
	}
	dom, dow := c.dom[t.Day()], c.dow[t.Weekday()]
	if c.orDays {
		return dom || dow
	}
	return dom && dow
}

func (c *cronSchedule) yearMatches(y int) bool {
	if c.year == nil {
		return true
	}
	return y >= 0 && y < len(c.year) && c.year[y]
}
