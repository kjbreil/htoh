package runner

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type Prog struct {
	mu   sync.RWMutex
	rows map[int]*Row
}

type Row struct {
	WorkerID  int
	FileBase  string
	Phase     string  // queued, probing, transcoding, done, failed
	FPS       float64 // from ffmpeg
	Speed     string  // e.g. "2.38x"
	OutTimeS  float64 // seconds encoded
	SizeBytes int64
	Device    string // VAAPI device path (e.g., "renderD128")
	Err       string
}

func NewProg() *Prog { return &Prog{rows: make(map[int]*Row)} }

func (p *Prog) Update(wid int, upd func(r *Row)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.rows[wid]
	if r == nil {
		r = &Row{WorkerID: wid}
		p.rows[wid] = r
	}
	upd(r)
}

func (p *Prog) Done(wid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.rows[wid]; r != nil {
		r.Phase = "done"
	}
}

func (p *Prog) Fail(wid int, err string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.rows[wid]; r != nil {
		r.Phase = "failed"
		r.Err = err
	}
}

func (p *Prog) RenderLoop(stop <-chan struct{}) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			p.renderOnce()
		case <-stop:
			p.renderOnce()
			return
		}
	}
}

func (p *Prog) renderOnce() {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Clear screen & move cursor home
	fmt.Fprint(os.Stdout, "\x1b[2J\x1b[H")

	// Collect rows stable ordered by worker id
	ids := make([]int, 0, len(p.rows))
	for id := range p.rows {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	// Check if any worker is using a device (VAAPI mode)
	hasDevice := false
	for _, r := range p.rows {
		if r.Device != "" {
			hasDevice = true
			break
		}
	}

	// Header
	fmt.Println("Workers live status (updates ~3/sec)")
	if hasDevice {
		fmt.Println(strings.Repeat("─", 105))
		fmt.Printf("%-6s  %-10s  %-28s  %7s  %6s  %8s  %8s  %-12s\n",
			"WID", "PHASE", "FILE", "FPS", "SPEED", "TIME", "SIZE", "DEVICE")
		fmt.Println(strings.Repeat("─", 105))
	} else {
		fmt.Println(strings.Repeat("─", 90))
		fmt.Printf("%-6s  %-10s  %-38s  %7s  %6s  %8s  %8s\n",
			"WID", "PHASE", "FILE", "FPS", "SPEED", "TIME", "SIZE")
		fmt.Println(strings.Repeat("─", 90))
	}

	// Rows
	for _, id := range ids {
		r := p.rows[id]
		file := r.FileBase
		maxFileLen := 38
		if hasDevice {
			maxFileLen = 28
		}
		if len(file) > maxFileLen {
			file = file[:maxFileLen-3] + "..."
		}
		times := fmt.Sprintf("%6.1fs", r.OutTimeS)
		size := humanBytes(r.SizeBytes)
		if r.Phase == "failed" && r.Err != "" {
			file = file + "  ✖ " + r.Err
		}

		if hasDevice {
			fmt.Printf("%-6d  %-10s  %-28s  %7.2f  %6s  %8s  %8s  %-12s\n",
				r.WorkerID, r.Phase, file, r.FPS, r.Speed, times, size, r.Device)
		} else {
			fmt.Printf("%-6d  %-10s  %-38s  %7.2f  %6s  %8s  %8s\n",
				r.WorkerID, r.Phase, file, r.FPS, r.Speed, times, size)
		}
	}

	if hasDevice {
		fmt.Println(strings.Repeat("─", 105))
	} else {
		fmt.Println(strings.Repeat("─", 90))
	}
	fmt.Println("Hints: -S to hide non-table logs | Ctrl+C stops after current tick")
}

func humanBytes(b int64) string {
	const k = 1024.0
	f := float64(b)
	switch {
	case f >= k*k*k:
		return fmt.Sprintf("%.1fGiB", f/(k*k*k))
	case f >= k*k:
		return fmt.Sprintf("%.1fMiB", f/(k*k))
	case f >= k:
		return fmt.Sprintf("%.1fKiB", f/k)
	default:
		return fmt.Sprintf("%dB", b)
	}
}
