package activity

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"
)

// Probe is one source of activity: the system (windows, idle), git repos, Claude sessions,
// later remote services (GitLab, GitHub, Jira, calendar). The collector calls Tick every
// Every(); a probe returns the spans that are ready to be written.
type Probe interface {
	Name() string
	Every() time.Duration
	Tick(ctx context.Context, now time.Time) ([]Span, error)
	// Flush returns what the probe still holds open, on shutdown.
	Flush(now time.Time) []Span
	// Info is a short line for the UI ("72 repos", "window titles: yes").
	Info() string
}

// ProbeStatus is what the UI shows per probe.
type ProbeStatus struct {
	Name     string `json:"name"`
	Info     string `json:"info,omitempty"`
	Error    string `json:"error,omitempty"`
	LastTick string `json:"lastTick,omitempty"`
}

type Collector struct {
	Log    Log
	Probes []Probe
	Now    func() time.Time

	mu     sync.Mutex
	status map[string]*ProbeStatus
}

func NewCollector(l Log, probes ...Probe) *Collector {
	c := &Collector{Log: l, Probes: probes, Now: time.Now, status: map[string]*ProbeStatus{}}
	for _, p := range probes {
		c.status[p.Name()] = &ProbeStatus{Name: p.Name()}
	}
	return c
}

func (c *Collector) Status() []ProbeStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []ProbeStatus{}
	for _, p := range c.Probes {
		s := *c.status[p.Name()]
		s.Info = p.Info()
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Run starts every probe in its own loop and blocks until ctx is done.
func (c *Collector) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, p := range c.Probes {
		wg.Add(1)
		go func(p Probe) {
			defer wg.Done()
			c.loop(ctx, p)
		}(p)
	}
	wg.Wait()
}

func (c *Collector) loop(ctx context.Context, p Probe) {
	t := time.NewTicker(p.Every())
	defer t.Stop()
	for {
		c.tick(ctx, p)
		select {
		case <-ctx.Done():
			c.write(p, p.Flush(c.Now()))
			return
		case <-t.C:
		}
	}
}

func (c *Collector) tick(ctx context.Context, p Probe) {
	now := c.Now()
	spans, err := p.Tick(ctx, now)
	if sn, ok := p.(Snapshot); ok {
		// a failed fetch keeps the last good snapshot; with several sources (accounts,
		// calendars) the ones that answered still replace it
		if err == nil || len(spans) > 0 {
			c.replace(p, sn.Days(now), spans)
		}
	} else {
		c.write(p, spans)
	}
	c.mu.Lock()
	st := c.status[p.Name()]
	st.LastTick = clock(now)
	st.Error = ""
	if err != nil {
		st.Error = err.Error()
	}
	c.mu.Unlock()
}

func (c *Collector) replace(p Probe, days []string, spans []Span) {
	byDay := map[string][]Span{}
	for _, s := range spans {
		s.Probe = p.Name()
		byDay[s.Date] = append(byDay[s.Date], s)
	}
	for _, d := range days {
		if err := c.Log.Replace(d, p.Name(), byDay[d]); err != nil {
			log.Printf("activity %s: write: %v", p.Name(), err)
		}
	}
}

func (c *Collector) write(p Probe, spans []Span) {
	for i := range spans {
		spans[i].Probe = p.Name()
	}
	if err := c.Log.Append(spans...); err != nil {
		log.Printf("activity %s: write: %v", p.Name(), err)
	}
}
