package video

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
)

type ProbeResult struct {
	Width           int
	Height          int
	DurationSeconds float64
}

type ffprobePayload struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func Probe(ctx context.Context, ffprobeBinary, sourcePath string) (ProbeResult, error) {
	cmd := exec.CommandContext(
		ctx,
		ffprobeBinary,
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		sourcePath,
	)

	output, err := cmd.Output()
	if err != nil {
		return ProbeResult{}, fmt.Errorf("run ffprobe: %w", err)
	}

	var payload ffprobePayload
	if err := json.Unmarshal(output, &payload); err != nil {
		return ProbeResult{}, fmt.Errorf("parse ffprobe output: %w", err)
	}
	if len(payload.Streams) == 0 {
		return ProbeResult{}, fmt.Errorf("ffprobe returned no video streams")
	}

	duration, err := strconv.ParseFloat(payload.Format.Duration, 64)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("parse duration: %w", err)
	}

	return ProbeResult{
		Width:           payload.Streams[0].Width,
		Height:          payload.Streams[0].Height,
		DurationSeconds: duration,
	}, nil
}
