package ingest

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"areuokay/internal/window"
)

const push = `{"messageId":1,"sessionId":"s","deviceId":"d","payload":[
	{"name":"totalacceleration","time":1700000000020000000,"values":{"x":0,"y":-9.80665,"z":0}},
	{"name":"gyroscope","time":1700000000010000000,"values":{"x":3.141592653589793,"y":0,"z":0}},
	{"name":"accelerometer","time":1700000000030000000,"values":{"x":1,"y":1,"z":1}},
	{"name":"location","time":1700000000040000000,"values":{}}]}`

func TestMergePrefersTotalAccelerationAndHoldsGyro(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(push), &msg); err != nil {
		t.Fatal(err)
	}
	got := (&merger{}).merge(msg.Payload)
	if len(got) != 1 {
		t.Fatalf("got %d samples, want 1 (accelerometer ignored once totalacceleration is seen): %+v", len(got), got)
	}
	s := got[0]
	if s.T != 1700000000020000000 || math.Abs(s.Ay+1) > 1e-9 || math.Abs(s.Gx-180) > 1e-9 {
		t.Fatalf("sample %+v, want ay=-1 g, gx=180 deg/s", s)
	}
}

func TestMergeAddsGravityBackToAccelerometer(t *testing.T) {
	var msg Message
	json.Unmarshal([]byte(`{"payload":[
		{"name":"accelerometer","time":1,"values":{"x":0,"y":0,"z":0}},
		{"name":"gravity","time":2,"values":{"x":0,"y":0,"z":-9.80665}},
		{"name":"accelerometer","time":3,"values":{"x":0,"y":0,"z":-9.80665}}]}`), &msg)
	got := (&merger{}).merge(msg.Payload)
	if len(got) != 1 || got[0].T != 3 || math.Abs(got[0].Az+2) > 1e-9 {
		t.Fatalf("got %+v, want one sample at t=3 with az=-2 g", got)
	}
}

func TestHandlerStatus(t *testing.T) {
	h := NewHandler(window.NewRegistry())
	cases := []struct {
		method, url, body string
		want              int
	}{
		{http.MethodPost, "/ingest?token=abc", push, http.StatusOK},
		{http.MethodPost, "/ingest", push, http.StatusUnauthorized},
		{http.MethodGet, "/ingest?token=abc", "", http.StatusMethodNotAllowed},
		{http.MethodPost, "/ingest?token=abc", "{", http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.url, strings.NewReader(c.body)))
		if rec.Code != c.want {
			t.Errorf("%s %s: status %d, want %d", c.method, c.url, rec.Code, c.want)
		}
	}
}
