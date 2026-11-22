package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A register keeps its requests in two lanes, each pruned oldest first on
// its own: normal requests, and anomalies (not answered 2xx, a failed
// verdict, a redelivery), so thousands of good requests never push out a
// failure. Pinned requests are in neither and never pruned by retention.

// Retention limits.
const (
	MaxKeep = 1_000_000
	MinAge  = time.Hour
	MaxAge  = 3650 * 24 * time.Hour
)

// Keep is the server's default counts: Normal from -retention, Anomalies
// from -retention-anomalies (0: no anomaly lane, anomalies are kept as
// normal requests, as before lanes existed).
type Keep struct {
	Normal, Anomalies int
}

// Lane is one lane's limits; a zero field means "the server's default"
// (Count) or "no age limit" (MaxAge).
type Lane struct {
	Count  int
	MaxAge time.Duration
}

// Retention is a register's own policy; the zero value is "the server's
// defaults".
type Retention struct {
	Normal, Anomalies Lane
	// AnomaliesSet tells an anomalies count of 0 (no lane) from an absent
	// one (the default).
	AnomaliesSet bool
}

// IsZero reports a policy that only takes the defaults.
func (p Retention) IsZero() bool { return p == Retention{} }

// Effective is a policy resolved against the server's defaults.
type Effective struct {
	Normal, Anomalies     int           // counts; Anomalies 0: one lane
	NormalAge, AnomalyAge time.Duration // 0: none
}

// Resolve fills a policy's absent fields from the defaults.
func (p Retention) Resolve(d Keep) Effective {
	e := Effective{Normal: d.Normal, Anomalies: d.Anomalies, NormalAge: p.Normal.MaxAge, AnomalyAge: p.Anomalies.MaxAge}
	if p.Normal.Count > 0 {
		e.Normal = p.Normal.Count
	}
	if p.AnomaliesSet {
		e.Anomalies = p.Anomalies.Count
	}
	return e
}

type laneJSON struct {
	Count  *int    `json:"count,omitempty"`
	MaxAge *string `json:"max_age,omitempty"`
}

type retentionJSON struct {
	Normal    *laneJSON `json:"normal,omitempty"`
	Anomalies *laneJSON `json:"anomalies,omitempty"`
}

// MarshalJSON writes the policy as {"normal": {"count", "max_age"},
// "anomalies": {...}}, absent fields left out.
func (p Retention) MarshalJSON() ([]byte, error) {
	var out retentionJSON
	lane := func(l Lane, countSet bool) *laneJSON {
		var j laneJSON
		if countSet {
			c := l.Count
			j.Count = &c
		}
		if l.MaxAge > 0 {
			a := FormatAge(l.MaxAge)
			j.MaxAge = &a
		}
		if j.Count == nil && j.MaxAge == nil {
			return nil
		}
		return &j
	}
	out.Normal = lane(p.Normal, p.Normal.Count > 0)
	out.Anomalies = lane(p.Anomalies, p.AnomaliesSet)
	return json.Marshal(out)
}

// UnmarshalJSON reads and validates a policy; unknown fields are refused.
func (p *Retention) UnmarshalJSON(b []byte) error {
	var in retentionJSON
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return fmt.Errorf("retention: %w", err)
	}
	var out Retention
	read := func(name string, j *laneJSON, l *Lane, min int) (bool, error) {
		if j == nil {
			return false, nil
		}
		set := false
		if j.Count != nil {
			if *j.Count < min || *j.Count > MaxKeep {
				return false, fmt.Errorf("%s.count is from %d to %d", name, min, MaxKeep)
			}
			l.Count, set = *j.Count, true
		}
		if j.MaxAge != nil {
			d, err := ParseAge(*j.MaxAge)
			if err != nil {
				return false, fmt.Errorf("%s.max_age: %w", name, err)
			}
			l.MaxAge = d
		}
		return set, nil
	}
	if _, err := read("normal", in.Normal, &out.Normal, 1); err != nil {
		return err
	}
	set, err := read("anomalies", in.Anomalies, &out.Anomalies, 0)
	if err != nil {
		return err
	}
	out.AnomaliesSet = set
	*p = out
	return nil
}

// ParseRetention reads a stored policy; "" is the defaults.
func ParseRetention(s string) (Retention, error) {
	var p Retention
	if strings.TrimSpace(s) == "" {
		return p, nil
	}
	err := json.Unmarshal([]byte(s), &p)
	return p, err
}

// String is the stored form ("" for the defaults).
func (p Retention) String() string {
	if p.IsZero() {
		return ""
	}
	b, _ := json.Marshal(p)
	return string(b)
}

// ParseAge reads an age such as 90m, 36h or 7d, from 1 h to 3650 d.
func ParseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return 0, errors.New("an age such as 90m, 36h or 7d")
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n < 1 {
		return 0, errors.New("an age such as 90m, 36h or 7d")
	}
	var unit time.Duration
	switch s[len(s)-1] {
	case 'm':
		unit = time.Minute
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	default:
		return 0, errors.New("an age such as 90m, 36h or 7d")
	}
	if int64(n) > int64(MaxAge/unit) {
		return 0, errors.New("at most 3650d")
	}
	d := time.Duration(n) * unit
	if d < MinAge {
		return 0, errors.New("at least 1h")
	}
	return d, nil
}

// FormatAge writes an age in its largest exact unit.
func FormatAge(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
}
