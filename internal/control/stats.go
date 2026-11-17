package control

import (
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/activity"
)

// Stats: how busy the simulator has been and how it has done. They are
// counted from the same request log as the Requests screen, so the two
// cannot disagree; nothing here is kept across a restart, as the log is not.

// BuildView says which simulator this is and what it is presenting.
type BuildView struct {
	// Version and Commit are what the process prints when it starts.
	Version string `json:"version"`
	Commit  string `json:"commit"`
	// VCenter is the vCenter version it pretends to be.
	VCenter string `json:"vcenter"`
}

// ServiceStat is what became of the requests for one service.
type ServiceStat struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	activity.Counts
	Requests uint64 `json:"requests"`
}

// StatBlock is one stretch of time: since the process started, or the last hour.
type StatBlock struct {
	Requests uint64 `json:"requests"`
	activity.Counts
	// MedianMs and SlowMs are the typical and slow-end response times in
	// milliseconds, rounded up to the edge of a timing bucket.
	MedianMs float64 `json:"medianMs"`
	SlowMs   float64 `json:"slowMs"`
	// MedianOver and SlowOver say that time was longer than the largest
	// bucket (30 s): the figure is then a lower bound, shown as "over".
	MedianOver bool          `json:"medianOver,omitempty"`
	SlowOver   bool          `json:"slowOver,omitempty"`
	Services   []ServiceStat `json:"services"`
	// CoveredSec is set only when the block covers less time than it is
	// named for: the request log holds a limited number of calls, so a busy
	// simulator's last hour reaches back only this far.
	CoveredSec int64 `json:"coveredSec,omitempty"`
}

// StatsView is the numbers on Home.
type StatsView struct {
	StartedAt time.Time `json:"startedAt"`
	UptimeSec int64     `json:"uptimeSec"`
	Total     StatBlock `json:"total"`
	LastHour  StatBlock `json:"lastHour"`
}

var serviceNames = map[string]string{
	activity.ServiceVSphere: "vSphere API", activity.ServiceVsan: "vSAN", activity.ServiceREST: "REST",
	activity.ServiceLookup: "Lookup", activity.ServiceLicense: "License", activity.ServiceSignOn: "Sign-on", activity.ServiceOther: "Other",
}

func statBlock(t activity.Tally) StatBlock {
	b := StatBlock{Requests: t.Total(), Counts: t.Counts, MedianMs: t.MedianMs, SlowMs: t.SlowMs, MedianOver: t.MedianOver, SlowOver: t.SlowOver, Services: []ServiceStat{}}
	for _, k := range activity.Services {
		if cs, ok := t.ByService[k]; ok && cs.Total() > 0 {
			b.Services = append(b.Services, ServiceStat{Key: k, Name: serviceNames[k], Counts: cs, Requests: cs.Total()})
		}
	}
	return b
}

// Stats counts what has been asked of the simulator. c.mu need not be held.
func (c *Controller) Stats() StatsView {
	now := c.now()
	hour := now.Add(-time.Hour)
	win, from := c.Log.Window(hour)
	last := statBlock(win)
	if from.After(hour) {
		last.CoveredSec = int64(now.Sub(from).Seconds())
	}
	return StatsView{StartedAt: c.started, UptimeSec: int64(now.Sub(c.started).Seconds()),
		Total: statBlock(c.Log.Lifetime()), LastHour: last}
}
