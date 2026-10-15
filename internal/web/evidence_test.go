package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cabv/internal/auth"
	"cabv/internal/store"
	qrcode "github.com/skip2/go-qrcode"
)

func TestAttendanceQRRealDecoderRoundTrip(t *testing.T) {
	payload := `{"v":1,"session_id":"11111111-1111-1111-1111-111111111111","token":"` + strings.Repeat("a", 43) + `","expires_at":"2026-10-14T20:00:00Z"}`
	imageData, err := qrDataPNG(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(imageData, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	image, err := png.Decode(bytes.NewReader(raw))
	if err != nil || image.Bounds().Dx() != 384 {
		t.Fatalf("QR PNG invalid %v", err)
	}
	if _, err := qrDataPNG(strings.Repeat("x", 513)); err == nil {
		t.Fatal("unbounded QR payload")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is needed to verify vendored QR decoder interoperability")
	}
	qr, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		t.Fatal(err)
	}
	matrix := qr.Bitmap()
	scale := 5
	width := len(matrix) * scale
	pixels := make([]byte, width*width*4)
	for y := 0; y < width; y++ {
		for x := 0; x < width; x++ {
			i := (y*width + x) * 4
			shade := byte(255)
			if matrix[y/scale][x/scale] {
				shade = 0
			}
			pixels[i] = shade
			pixels[i+1] = shade
			pixels[i+2] = shade
			pixels[i+3] = 255
		}
	}
	input, _ := json.Marshal(map[string]any{"pixels": base64.StdEncoding.EncodeToString(pixels), "width": width, "payload": payload})
	command := exec.Command(node, "-e", `const fs=require('node:fs');const decode=require('./static/jsQR.js');const input=JSON.parse(fs.readFileSync(0,'utf8'));const data=new Uint8ClampedArray(Buffer.from(input.pixels,'base64'));const found=decode(data,input.width,input.width);if(!found||found.data!==input.payload)throw new Error('QR round trip failed');`)
	command.Stdin = bytes.NewReader(input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("actual decoder roundtrip %v: %s", err, output)
	}
}

func TestSensorBrowserHelpers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is needed to test browser sensor helpers")
	}
	if output, err := exec.Command(node, "--test", "tests/sensors.test.cjs").CombinedOutput(); err != nil {
		t.Fatalf("sensor browser helper tests %v: %s", err, output)
	}
}

func TestSensorHTTPStrictInputAndPrivacy(t *testing.T) {
	pool := platformDB(t)
	database := &store.Store{Pool: pool}
	ctx := context.Background()
	ids := map[string]string{}
	cookies := map[string]*http.Cookie{}
	for _, role := range []string{"admin", "lecturer", "student", "outsider"} {
		actual := role
		if role == "outsider" {
			actual = "student"
		}
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO users(email,name,role,password_hash) VALUES($1,$1,$2,'unused') RETURNING id::text`, role+"@sensor.test", actual).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[role] = id
		token, err := auth.Token()
		if err != nil {
			t.Fatal(err)
		}
		if err := database.CreateSession(ctx, id, auth.HashToken(token), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		cookies[role] = &http.Cookie{Name: "cabv_session", Value: token}
	}
	var course, room string
	if err := pool.QueryRow(ctx, `INSERT INTO courses(code,title) VALUES('SEN101','Sensor test') RETURNING id::text`).Scan(&course); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO rooms(name,kind,capacity) VALUES('Sensor room','room',50) RETURNING id::text`).Scan(&room); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO course_lecturers(course_id,lecturer_id) VALUES($1,$2)`, course, ids["lecturer"]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO enrollments(course_id,student_id) VALUES($1,$2)`, course, ids["student"]); err != nil {
		t.Fatal(err)
	}
	lecturer := store.User{ID: ids["lecturer"]}
	session, err := database.CreateAttendanceSession(ctx, lecturer, course, room, "Sensors", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenAttendanceSession(ctx, lecturer, session.ID); err != nil {
		t.Fatal(err)
	}
	a := &application{Options: Options{Store: database}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/{id}/evidence", a.recordEvidence)
	mux.HandleFunc("GET /sessions/{id}/evidence", a.attendanceEvidence)
	mux.HandleFunc("POST /sessions/{id}/calibration", a.calibrateRoom)
	request := func(role, method, path, body string, csrf bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(cookies[role])
		r.Header.Set("Content-Type", "application/json")
		if csrf {
			r.Header.Set("X-CSRF-Token", auth.CSRF(cookies[role].Value))
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	path := "/sessions/" + session.ID
	body := `{"consent":true,"capture_method":"qr","gps":{"latitude":6.5,"longitude":3.4,"accuracy_meters":50}}`
	if w := request("student", "POST", path+"/evidence", body, false); w.Code != 403 {
		t.Fatalf("CSRF %d", w.Code)
	}
	for _, bad := range []string{`null`, `{"consent":true,"capture_method":"qr","face":"raw"}`, `{"consent":false,"capture_method":"qr"}`, `{} {}`} {
		if w := request("student", "POST", path+"/evidence", bad, true); w.Code != 400 {
			t.Fatalf("input %s: %d %s", bad, w.Code, w.Body.String())
		}
	}
	if w := request("student", "POST", path+"/evidence", `{"unknown":"`+strings.Repeat("x", 5000)+`"}`, true); w.Code != 413 {
		t.Fatalf("size %d", w.Code)
	}
	if w := request("outsider", "POST", path+"/evidence", body, true); w.Code != 404 {
		t.Fatalf("outsider %d", w.Code)
	}
	if w := request("student", "POST", path+"/evidence", body, true); w.Code != 201 {
		t.Fatalf("capture %d %s", w.Code, w.Body.String())
	}
	for _, role := range []string{"student", "lecturer"} {
		w := request(role, "GET", path+"/evidence", "", false)
		if w.Code != 200 || strings.Contains(w.Body.String(), "latitude") || strings.Contains(w.Body.String(), "longitude") || !strings.Contains(w.Body.String(), "shadow") {
			t.Fatalf("privacy %d %s", w.Code, w.Body.String())
		}
	}
	calibration := `{"consent":true,"spectral_features":[0.05,0.1,0.1,0.15,0.2,0.2,0.15,0.05]}`
	if w := request("student", "POST", path+"/calibration", calibration, true); w.Code != 404 {
		t.Fatalf("student calibration %d", w.Code)
	}
	if w := request("lecturer", "POST", path+"/calibration", calibration, true); w.Code != 200 {
		t.Fatalf("manager calibration %d %s", w.Code, w.Body.String())
	}
}
