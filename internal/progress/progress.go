package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Bar struct {
	out          io.Writer
	total        int64
	done         atomic.Int64
	rateBase     atomic.Int64
	labelMu      sync.RWMutex
	renderMu     sync.Mutex
	label        string
	labelVersion atomic.Int64
	interactive  bool
	stop         chan struct{}
	stopped      chan struct{}
	started      time.Time
	once         sync.Once
	lastDone     int64
	lastVersion  int64
}

func New(out io.Writer, total int64, label string) *Bar {
	b := &Bar{out: out, total: total, label: label, stop: make(chan struct{}), stopped: make(chan struct{}), started: time.Now(), lastDone: -1}
	if f, ok := out.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			b.interactive = true
		}
	}
	go b.loop()
	return b
}

func (b *Bar) Add(n int64) { b.done.Add(n) }

func (b *Bar) AddCompleted(n int64) {
	b.done.Add(n)
	b.rateBase.Add(n)
}

func (b *Bar) Done() int64 { return b.done.Load() }

func (b *Bar) SetDone(n int64) {
	b.done.Store(n)
	b.rateBase.Store(n)
}

func (b *Bar) SetLabel(label string) {
	b.labelMu.Lock()
	b.label = label
	b.labelMu.Unlock()
	b.labelVersion.Add(1)
}

func (b *Bar) Finish() {
	b.once.Do(func() {
		close(b.stop)
		<-b.stopped
	})
}

func (b *Bar) Logf(format string, args ...any) {
	b.renderMu.Lock()
	defer b.renderMu.Unlock()
	if b.interactive {
		fmt.Fprint(b.out, "\r\x1b[2K")
	}
	fmt.Fprintf(b.out, format, args...)
}

func (b *Bar) loop() {
	defer close(b.stopped)
	interval := time.Second
	if b.interactive {
		interval = 120 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	b.render(false)
	for {
		select {
		case <-ticker.C:
			b.render(false)
		case <-b.stop:
			b.render(true)
			return
		}
	}
}

func (b *Bar) render(final bool) {
	b.renderMu.Lock()
	defer b.renderMu.Unlock()
	done := b.done.Load()
	version := b.labelVersion.Load()
	if !b.interactive && done == b.lastDone && version == b.lastVersion {
		return
	}
	b.lastDone = done
	b.lastVersion = version
	if done > b.total && b.total >= 0 {
		done = b.total
	}
	b.labelMu.RLock()
	label := b.label
	b.labelMu.RUnlock()
	elapsed := time.Since(b.started).Seconds()
	var rate float64
	if elapsed > 0 {
		rate = float64(done-b.rateBase.Load()) / elapsed
		if rate < 0 {
			rate = 0
		}
	}
	line := ""
	if b.total > 0 {
		pct := float64(done) * 100 / float64(b.total)
		if b.interactive {
			width := 24
			filled := int(pct * float64(width) / 100)
			if filled > width {
				filled = width
			}
			line = fmt.Sprintf("%-28.28s [%s%s] %6.2f%% %s/%s %s/s",
				label, strings.Repeat("█", filled), strings.Repeat("░", width-filled), pct,
				formatBytes(done), formatBytes(b.total), formatBytes(int64(rate)))
		} else {
			line = fmt.Sprintf("%s: %6.2f%% %s/%s %s/s", label, pct, formatBytes(done), formatBytes(b.total), formatBytes(int64(rate)))
		}
	} else {
		line = fmt.Sprintf("%s: %s %s/s", label, formatBytes(done), formatBytes(int64(rate)))
	}
	if b.interactive {
		fmt.Fprintf(b.out, "\r\x1b[2K%s", line)
		if final {
			fmt.Fprintln(b.out)
		}
	} else if final || done > 0 {
		fmt.Fprintln(b.out, line)
	}
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 5; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
