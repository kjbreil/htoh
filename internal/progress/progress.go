package progress

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// UI rendering constants.
	renderIntervalMs = 300 // milliseconds between UI updates

	// UI layout constants.
	tableWidthWithDevice    = 105
	tableWidthWithoutDevice = 90
	maxFileNameWithDevice   = 28
	maxFileNameDefault      = 38
	fileNameTruncateSuffix  = 3 // characters for "..."

	// Byte size conversion.
	bytesPerKiB = 1024.0
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
	StartTime time.Time // when transcoding started
	DurationS float64   // total video duration in seconds
}

func NewProg() *Prog {
	return &Prog{
		mu:   sync.RWMutex{},
		rows: make(map[int]*Row),
	}
}

func (p *Prog) Update(wid int, upd func(r *Row)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.rows[wid]
	if r == nil {
		r = &Row{
			WorkerID:  wid,
			FileBase:  "",
			Phase:     "",
			FPS:       0,
			Speed:     "",
			OutTimeS:  0,
			SizeBytes: 0,
			Device:    "",
			Err:       "",
			StartTime: time.Time{},
			DurationS: 0,
		}
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

// GetProgress returns a copy of the current progress for a worker.
func (p *Prog) GetProgress(wid int) *Row {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r := p.rows[wid]
	if r == nil {
		return nil
	}
	// Return a copy to avoid race conditions
	rowCopy := *r
	return &rowCopy
}

func (p *Prog) RenderLoop(stop <-chan struct{}) {
	t := time.NewTicker(renderIntervalMs * time.Millisecond)
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
	//nolint:forbidigo // Live dashboard UI output to stdout
	fmt.Println("Workers live status (updates ~3/sec)")
	if hasDevice {
		//nolint:forbidigo // Live dashboard UI output to stdout
		fmt.Println(strings.Repeat("─", tableWidthWithDevice))
		//nolint:forbidigo // Live dashboard UI output to stdout
		fmt.Printf("%-6s  %-10s  %-28s  %7s  %6s  %8s  %8s  %-12s\n",
			"WID", "PHASE", "FILE", "FPS", "SPEED", "TIME", "SIZE", "DEVICE")
		//nolint:forbidigo // Live dashboard UI output to stdout
		fmt.Println(strings.Repeat("─", tableWidthWithDevice))
	} else {
		//nolint:forbidigo // Live dashboard UI output to stdout
		fmt.Println(strings.Repeat("─", tableWidthWithoutDevice))
		//nolint:forbidigo // Live dashboard UI output to stdout
		fmt.Printf("%-6s  %-10s  %-38s  %7s  %6s  %8s  %8s\n",
			"WID", "PHASE", "FILE", "FPS", "SPEED", "TIME", "SIZE")
		//nolint:forbidigo // Live dashboard UI output to stdout
		fmt.Println(strings.Repeat("─", tableWidthWithoutDevice))
	}

	// Rows
	for _, id := range ids {
		r := p.rows[id]
		file := r.FileBase
		maxFileLen := maxFileNameDefault
		if hasDevice {
			maxFileLen = maxFileNameWithDevice
		}
		if len(file) > maxFileLen {
			file = file[:maxFileLen-fileNameTruncateSuffix] + "..."
		}
		times := fmt.Sprintf("%6.1fs", r.OutTimeS)
		size := humanBytes(r.SizeBytes)
		if r.Phase == "failed" && r.Err != "" {
			file = file + "  ✖ " + r.Err
		}

		if hasDevice {
			//nolint:forbidigo // Live dashboard UI output to stdout
			fmt.Printf("%-6d  %-10s  %-28s  %7.2f  %6s  %8s  %8s  %-12s\n",
				r.WorkerID, r.Phase, file, r.FPS, r.Speed, times, size, r.Device)
		} else {
			//nolint:forbidigo // Live dashboard UI output to stdout
			fmt.Printf("%-6d  %-10s  %-38s  %7.2f  %6s  %8s  %8s\n",
				r.WorkerID, r.Phase, file, r.FPS, r.Speed, times, size)
		}
	}

	if hasDevice {
		//nolint:forbidigo // Live dashboard UI output to stdout
		fmt.Println(strings.Repeat("─", tableWidthWithDevice))
	} else {
		//nolint:forbidigo // Live dashboard UI output to stdout
		fmt.Println(strings.Repeat("─", tableWidthWithoutDevice))
	}
	//nolint:forbidigo // Live dashboard UI output to stdout
	fmt.Println("Hints: -S to hide non-table logs | Ctrl+C stops after current tick")
}

func humanBytes(b int64) string {
	f := float64(b)
	switch {
	case f >= bytesPerKiB*bytesPerKiB*bytesPerKiB:
		return fmt.Sprintf("%.1fGiB", f/(bytesPerKiB*bytesPerKiB*bytesPerKiB))
	case f >= bytesPerKiB*bytesPerKiB:
		return fmt.Sprintf("%.1fMiB", f/(bytesPerKiB*bytesPerKiB))
	case f >= bytesPerKiB:
		return fmt.Sprintf("%.1fKiB", f/bytesPerKiB)
	default:
		return fmt.Sprintf("%dB", b)
	}
}
