// Command replay streams a recording to /ingest at real speed, in the same
// JSON format Sensor Logger pushes, one batch per second of data.
//
// It reads:
//   - a SisFall .txt file (200 Hz, raw ADXL345 + ITG3200 counts)
//   - a Sensor Logger CSV export folder (TotalAcceleration.csv, Gyroscope.csv, …)
//   - a single Sensor Logger CSV file, named after its sensor
package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const standardGravity = 9.80665

// SisFall conversion from its readme: ADXL345 ±16 g over 13 bits,
// ITG3200 ±2000 °/s over 16 bits. SisFall samples at 200 Hz.
const (
	sisfallAccelG   = 2.0 * 16 / (1 << 13)
	sisfallGyroDegS = 2.0 * 2000 / (1 << 16)
	sisfallPeriod   = int64(5 * time.Millisecond)
)

// sensorLoggerNames are the streams the core understands, keyed by lowercased
// CSV base name.
var sensorLoggerNames = map[string]bool{
	"totalacceleration": true, "accelerometer": true, "gravity": true, "gyroscope": true,
}

type entry struct {
	Name   string `json:"name"`
	Time   int64  `json:"time"`
	Values struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		Z float64 `json:"z"`
	} `json:"values"`
}

type message struct {
	MessageID int     `json:"messageId"`
	SessionID string  `json:"sessionId"`
	DeviceID  string  `json:"deviceId"`
	Payload   []entry `json:"payload"`
}

func main() {
	url := flag.String("url", "http://localhost:8080/ingest?token=replay", "ingest URL including token")
	speed := flag.Float64("speed", 1, "playback speed; 1 = real time")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: replay [flags] <sisfall.txt | sensor-logger-folder | sensor.csv>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	entries, err := load(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	if len(entries) == 0 {
		log.Fatal("no readings found")
	}

	base := entries[0].Time
	shift := time.Now().UnixNano() - base
	start := time.Now()
	client := &http.Client{Timeout: 5 * time.Second}
	dur := time.Duration(entries[len(entries)-1].Time - base)
	log.Printf("replaying %s (%s of data, %d readings) at %gx to %s", flag.Arg(0), dur.Round(time.Millisecond), len(entries), *speed, *url)

	for i, n := 0, 0; i < len(entries); n++ {
		chunkEnd := entries[i].Time + int64(time.Second)
		j := i
		for j < len(entries) && entries[j].Time < chunkEnd {
			j++
		}
		// Send each second once it has "happened", like the phone does.
		time.Sleep(time.Until(start.Add(time.Duration(float64(chunkEnd-base) / *speed))))
		chunk := make([]entry, j-i)
		for k, e := range entries[i:j] {
			e.Time += shift
			chunk[k] = e
		}
		if err := post(client, *url, message{MessageID: n, SessionID: "replay", DeviceID: "replay", Payload: chunk}); err != nil {
			log.Printf("batch %d: %v", n, err)
		}
		fmt.Printf("\rsent %s / %s", time.Duration(entries[j-1].Time-base).Round(time.Second), dur.Round(time.Second))
		i = j
	}
	fmt.Println()
	log.Printf("done")
}

func post(c *http.Client, url string, m message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	resp, err := c.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}

func load(path string) ([]entry, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	var entries []entry
	switch {
	case info.IsDir():
		files, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if name := sensorName(f.Name()); sensorLoggerNames[name] {
				es, err := loadSensorLoggerCSV(filepath.Join(path, f.Name()), name)
				if err != nil {
					return nil, err
				}
				entries = append(entries, es...)
			}
		}
	case strings.EqualFold(filepath.Ext(path), ".csv"):
		name := sensorName(path)
		if !sensorLoggerNames[name] {
			return nil, fmt.Errorf("%s: file name must be one of TotalAcceleration, Accelerometer, Gravity, Gyroscope", path)
		}
		entries, err = loadSensorLoggerCSV(path, name)
	default:
		entries, err = loadSisFall(path)
	}
	if err != nil {
		return nil, err
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Time < entries[j].Time })
	return entries, nil
}

func sensorName(path string) string {
	b := filepath.Base(path)
	return strings.ToLower(strings.TrimSuffix(b, filepath.Ext(b)))
}

// loadSisFall reads lines like "17,-179,-99,-18,-504,-352,76,-697,-279;".
// Only the first six columns are used: ADXL345 accel and ITG3200 gyro.
func loadSisFall(path string) ([]entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []entry
	sc := bufio.NewScanner(f)
	for i, line := 0, 0; sc.Scan(); line++ {
		fields := strings.Split(strings.Trim(strings.TrimSpace(sc.Text()), ";"), ",")
		if len(fields) < 6 {
			continue
		}
		var raw [6]float64
		for k := range raw {
			raw[k], err = strconv.ParseFloat(strings.TrimSpace(fields[k]), 64)
			if err != nil {
				return nil, fmt.Errorf("%s line %d: %v", path, line+1, err)
			}
		}
		t := int64(i) * sisfallPeriod
		a := entry{Name: "totalacceleration", Time: t}
		a.Values.X, a.Values.Y, a.Values.Z = raw[0]*sisfallAccelG*standardGravity, raw[1]*sisfallAccelG*standardGravity, raw[2]*sisfallAccelG*standardGravity
		g := entry{Name: "gyroscope", Time: t}
		toRad := sisfallGyroDegS * math.Pi / 180
		g.Values.X, g.Values.Y, g.Values.Z = raw[3]*toRad, raw[4]*toRad, raw[5]*toRad
		out = append(out, g, a) // gyro first, so the accel sample picks it up
		i++
	}
	return out, sc.Err()
}

// loadSensorLoggerCSV reads a Sensor Logger export with a header containing
// time (unix ns), x, y and z, in any column order.
func loadSensorLoggerCSV(path, name string) ([]entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if len(rows) < 2 {
		return nil, nil
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, k := range []string{"time", "x", "y", "z"} {
		if _, ok := col[k]; !ok {
			return nil, fmt.Errorf("%s: missing column %q", path, k)
		}
	}
	out := make([]entry, 0, len(rows)-1)
	for n, r := range rows[1:] {
		e := entry{Name: name}
		var errs [4]error
		e.Time, errs[0] = strconv.ParseInt(r[col["time"]], 10, 64)
		e.Values.X, errs[1] = strconv.ParseFloat(r[col["x"]], 64)
		e.Values.Y, errs[2] = strconv.ParseFloat(r[col["y"]], 64)
		e.Values.Z, errs[3] = strconv.ParseFloat(r[col["z"]], 64)
		for _, err := range errs {
			if err != nil {
				return nil, fmt.Errorf("%s row %d: %v", path, n+2, err)
			}
		}
		out = append(out, e)
	}
	return out, nil
}
