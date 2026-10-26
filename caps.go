package main

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// Cost caps: an API token is blocked when its incoming costs (API prices) reach its cap, a server is
// taken out of rotation when its outgoing costs (server prices) reach its cap. The cap applies per
// period – day, month or in total – and is lifted automatically when a new period starts.

const (
	PeriodDay   = "day"
	PeriodMonth = "month"
	PeriodTotal = "total"
)

func normalizePeriod(p string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "", PeriodMonth, "monat":
		return PeriodMonth, nil
	case PeriodDay, "tag":
		return PeriodDay, nil
	case PeriodTotal, "gesamt":
		return PeriodTotal, nil
	}
	return "", fmt.Errorf("Zeitraum %q: erlaubt sind day, month, total", p)
}

func periodLabel(p string) string {
	switch p {
	case PeriodDay:
		return "pro Tag"
	case PeriodTotal:
		return "gesamt"
	}
	return "pro Monat"
}

// periodStart returns the first day key of the current period ("" = no limit).
func periodStart(p string, now time.Time) string {
	now = now.In(time.Local)
	switch p {
	case PeriodDay:
		return dayKey(now)
	case PeriodMonth:
		return now.Format("2006-01") + "-01"
	}
	return ""
}

// BackendSpend: outgoing costs of a server since the start of the period (from the daily statistics).
func (s *Stats) BackendSpend(name, from string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := 0.0
	for day, m := range s.days {
		if day >= from {
			if st := m[name]; st != nil {
				sum += st.Cost
			}
		}
	}
	return sum
}

// TokenSpend: incoming costs of an API token since the start of the period.
func (s *Stats) TokenSpend(name, from string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := 0.0
	for day, m := range s.tokdays {
		if day >= from {
			if st := m[name]; st != nil {
				sum += st.Cost
			}
		}
	}
	return sum
}

type capTarget struct {
	name   string
	cap    float64
	period string
}

// checkCaps recomputes the spending of every capped token and server and blocks/unblocks them.
// Safe to call often (after every request and every few seconds).
func (p *Pool) checkCaps() {
	if p.stats == nil {
		return
	}
	now := time.Now()

	// servers
	p.mu.Lock()
	var servers []capTarget
	for _, b := range p.backends {
		if b.costCap > 0 {
			servers = append(servers, capTarget{b.Name, b.costCap, b.capPeriod})
		}
	}
	p.mu.Unlock()
	spent := map[string]float64{}
	for _, t := range servers {
		spent[t.name] = p.stats.BackendSpend(t.name, periodStart(t.period, now))
	}
	var events []Event
	p.mu.Lock()
	changed := false
	for _, b := range p.backends {
		v, ok := spent[b.Name]
		capped := ok && b.costCap > 0 && v >= b.costCap
		b.capSpent = v
		if capped == b.capped {
			continue
		}
		b.capped, changed = capped, true
		if capped {
			log.Printf("backend %s: cost cap reached (%.4f of %.4f %s) – taken out of rotation", b.Name, v, b.costCap, periodLabel(b.capPeriod))
			events = append(events, Event{Kind: EvServerCapped, Object: b.Name,
				Text: fmt.Sprintf("Server %s hat den Kostendeckel erreicht: %s von %s %s. Er bekommt keine neuen Anfragen mehr.",
					b.Name, moneyText(v, p.cfg.Currency), moneyText(b.costCap, p.cfg.Currency), periodLabel(b.capPeriod))})
		} else {
			log.Printf("backend %s: below cost cap again – back in rotation", b.Name)
			events = append(events, Event{Kind: EvServerUncapped, Object: b.Name,
				Text: fmt.Sprintf("Server %s ist wieder unter dem Kostendeckel (neuer Zeitraum oder Deckel geändert) und bekommt wieder Anfragen.", b.Name)})
		}
	}
	if changed {
		p.dispatchLocked() // capped: waiting requests go to the fallback; uncapped: it can take work
	}
	p.mu.Unlock()

	// API tokens
	if p.access != nil {
		events = append(events, p.access.checkCaps(p.stats, now, p.cfg.Currency)...)
	}
	for _, e := range events {
		p.emit(e)
	}
}

func (a *Access) checkCaps(st *Stats, now time.Time, currency string) []Event {
	a.mu.Lock()
	var targets []capTarget
	for _, t := range a.tokens {
		if t.CostCap > 0 {
			targets = append(targets, capTarget{t.Name, t.CostCap, t.CapPeriod})
		}
	}
	totals := map[string]float64{}
	for _, t := range a.tokens {
		totals[t.Name] = t.Cost
	}
	a.mu.Unlock()
	spent := map[string]float64{}
	for _, t := range targets {
		if t.period == PeriodTotal {
			spent[t.name] = totals[t.name] // lifetime counter, same as "Einnahmen gesamt"
		} else {
			spent[t.name] = st.TokenSpend(t.name, periodStart(t.period, now))
		}
	}
	var events []Event
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.tokens {
		v, ok := spent[t.Name]
		capped := ok && t.CostCap > 0 && v >= t.CostCap
		t.capSpent = v
		if capped == t.capped {
			continue
		}
		t.capped = capped
		if capped {
			log.Printf("access token %q: cost cap reached (%.4f of %.4f %s) – blocked", t.Name, v, t.CostCap, periodLabel(t.CapPeriod))
			events = append(events, Event{Kind: EvTokenCapped, Object: t.Name,
				Text: fmt.Sprintf("API-Token %s hat den Kostendeckel erreicht: %s von %s %s. Anfragen mit diesem Token werden abgelehnt.",
					t.Name, moneyText(v, currency), moneyText(t.CostCap, currency), periodLabel(t.CapPeriod))})
		} else {
			log.Printf("access token %q: below cost cap again – unblocked", t.Name)
			events = append(events, Event{Kind: EvTokenUncapped, Object: t.Name,
				Text: fmt.Sprintf("API-Token %s ist wieder unter dem Kostendeckel (neuer Zeitraum oder Deckel geändert) und freigegeben.", t.Name)})
		}
	}
	return events
}

func moneyText(v float64, currency string) string {
	s := strings.Replace(fmt.Sprintf("%.2f", v), ".", ",", 1)
	if v > 0 && v < 0.01 {
		s = strings.Replace(fmt.Sprintf("%.4f", v), ".", ",", 1)
	}
	return s + " " + currency
}

// capsLoop re-checks the caps regularly (new day/month lifts caps) and right after requests finished.
func (p *Pool) capsLoop(stop <-chan struct{}) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		case <-p.capKick:
		}
		p.checkCaps()
	}
}

func (p *Pool) kickCaps() {
	select {
	case p.capKick <- struct{}{}:
	default:
	}
}
