package garmin

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const DownloadDirEnvVar = "GARMIN_MCP_DOWNLOAD_DIR"

type BoundingBox struct {
	North, South, East, West float64
}

// NewBoundingBox returns nil when no edge is given. Garmin only filters by
// start location with all four edges.
func NewBoundingBox(north, south, east, west *float64) (*BoundingBox, error) {
	given := 0
	for _, edge := range []*float64{north, south, east, west} {
		if edge != nil {
			given++
		}
	}
	switch given {
	case 0:
		return nil, nil
	case 4:
		return &BoundingBox{North: *north, South: *south, East: *east, West: *west}, nil
	default:
		return nil, errors.New("garmin filters by start location only with all four edges: " +
			"pass north, south, east, and west latitudes/longitudes, or none of them")
	}
}

type ActivitySearch struct {
	Start, End               *time.Time
	Search                   string
	ActivityType             string
	ActivitySubtype          string
	ExcludedSubtypes         []string
	EventType                string
	Favorite                 *bool
	ExcludeChildren          bool
	SortBy                   string
	SortOrder                string
	MinDistance, MaxDistance *int
	MinDuration, MaxDuration *int
	MinElapsedDuration       *int
	MaxElapsedDuration       *int
	MinElevation             *int
	MaxElevation             *int
	MinAvgPower              *int
	StartArea                *BoundingBox
	MinMaxDepth, MaxMaxDepth *int
	DecoDive                 *bool
	MinBottomTime            *int
	MaxBottomTime            *int
	RequiredFields           []string
	Limit, Offset            int
}

func (s ActivitySearch) values() query {
	q := query{}.
		integer("start", &s.Offset).
		integer("limit", &s.Limit).
		str("search", s.Search).
		str("activityType", s.ActivityType).
		str("activitySubType", s.ActivitySubtype).
		strs("excludedActivitySubType", s.ExcludedSubtypes).
		str("eventType", s.EventType).
		boolean("excludeChildren", &s.ExcludeChildren).
		str("sortBy", s.SortBy).
		str("sortOrder", s.SortOrder).
		integer("minDistance", s.MinDistance).
		integer("maxDistance", s.MaxDistance).
		integer("minDuration", s.MinDuration).
		integer("maxDuration", s.MaxDuration).
		integer("minElapsedDuration", s.MinElapsedDuration).
		integer("maxElapsedDuration", s.MaxElapsedDuration).
		integer("minElevation", s.MinElevation).
		integer("maxElevation", s.MaxElevation).
		integer("minAvgPower", s.MinAvgPower).
		integer("lowerMaxDepth", s.MinMaxDepth).
		integer("upperMaxDepth", s.MaxMaxDepth).
		boolean("decoDive", s.DecoDive).
		integer("minBottomTime", s.MinBottomTime).
		integer("maxBottomTime", s.MaxBottomTime).
		strs("requiredField", s.RequiredFields)
	if s.Start != nil {
		q.date("startDate", *s.Start)
	}
	if s.End != nil {
		q.date("endDate", *s.End)
	}
	// Garmin takes favorite as 1 or 0 rather than a boolean.
	if s.Favorite != nil {
		favorite := 0
		if *s.Favorite {
			favorite = 1
		}
		q.integer("favorite", &favorite)
	}
	if box := s.StartArea; box != nil {
		q.number("upperLatitude", &box.North).
			number("lowerLatitude", &box.South).
			number("rightLongitude", &box.East).
			number("leftLongitude", &box.West)
	}
	return q
}

func (c *Client) SearchActivities(ctx context.Context, search ActivitySearch) (any, error) {
	return c.Get(ctx, "/activitylist-service/activities/search/activities", search.values().values())
}

var errActivityID = errors.New("activity_id must be a positive integer")

func (c *Client) Activity(ctx context.Context, activityID int64) (any, error) {
	if activityID <= 0 {
		return nil, errActivityID
	}
	return c.Get(ctx, "/activity-service/activity/"+strconv.FormatInt(activityID, 10), nil)
}

func (c *Client) ActivityWorkouts(ctx context.Context, activityID int64) (any, error) {
	if activityID <= 0 {
		return nil, errActivityID
	}
	return c.Get(ctx, fmt.Sprintf("/activity-service/activity/%d/workouts", activityID), nil)
}

type DownloadedFile struct {
	Path  string   `json:"path"`
	Bytes int      `json:"bytes"`
	Files []string `json:"files"`
}

// DownloadActivity saves the original activity ZIP as
// activity-<id>.zip in directory, replacing an earlier download.
func (c *Client) DownloadActivity(ctx context.Context, activityID int64, directory string) (*DownloadedFile, error) {
	if activityID <= 0 {
		return nil, errActivityID
	}
	content, err := c.Download(ctx, "/download-service/files/activity/"+strconv.FormatInt(activityID, 10))
	if err != nil {
		return nil, err
	}
	archive, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return nil, fmt.Errorf("garmin's activity file is not a ZIP: %w", err)
	}
	files := make([]string, 0, len(archive.File))
	for _, file := range archive.File {
		files = append(files, file.Name)
	}
	path := filepath.Join(directory, fmt.Sprintf("activity-%d.zip", activityID))
	if err := writeReplacing(path, content); err != nil {
		return nil, err
	}
	return &DownloadedFile{Path: path, Bytes: len(content), Files: files}, nil
}

// writeReplacing writes through a temp file so a failed download never leaves
// a truncated ZIP behind. os.Rename replaces an existing file on every OS Go
// supports, including Windows.
func writeReplacing(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(content); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// DownloadDir expands a leading ~ itself because MCP clients pass env values
// without a shell.
func DownloadDir() (string, error) {
	dir := os.Getenv(DownloadDirEnvVar)
	if dir != "" && dir != "~" && !strings.HasPrefix(dir, "~/") && !strings.HasPrefix(dir, `~\`) {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if dir == "" {
		return filepath.Join(home, "Downloads", "garmin"), nil
	}
	return filepath.Join(home, dir[1:]), nil
}

// DownloadTarget confines a caller-chosen subfolder to the download folder.
// filepath.IsLocal also rejects Windows drive letters and reserved names such
// as NUL.
func DownloadTarget(subdir string) (string, error) {
	base, err := DownloadDir()
	if err != nil || subdir == "" {
		return base, err
	}
	if !filepath.IsLocal(subdir) {
		return "", fmt.Errorf("directory must be a subfolder inside %s, such as \"races\"; "+
			"set %s to change the download folder", base, DownloadDirEnvVar)
	}
	return filepath.Join(base, subdir), nil
}
