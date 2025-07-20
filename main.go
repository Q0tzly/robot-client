package main

import (
    "bufio"
    "encoding/json"
    "fmt"
    "log"
    "net/http"
    "os/exec"
    "sync"
)

type Message struct {
    Message string `json:"message"`
}

type MoveRequest struct {
    Direction string `json:"direction"`
}

var (
    latestFrame  []byte
    frameMutex   sync.RWMutex
    frameUpdated = make(chan struct{}, 1)

    currentDirection = ""
    isMoving        = false
)

func captureLoop() {
    cmd := exec.Command("bash", "-c", `
        libcamera-vid -t 0 --inline --width 1280 --height 720 --framerate 24 --bitrate 4000000 --buffer-count 2 -o - |
        ffmpeg -fflags nobuffer -flags low_delay -i - -q:v 2 -pix_fmt yuvj422p -f mjpeg pipe:1
    `)

    stdout, err := cmd.StdoutPipe()
    if err != nil {
        log.Fatal("Failed to get stdout pipe:", err)
    }

    if err := cmd.Start(); err != nil {
        log.Fatal("Failed to start capture process:", err)
    }

    reader := bufio.NewReader(stdout)

    for {
        frame, err := readNextJPEG(reader)
        if err != nil {
            log.Println("Error reading JPEG frame:", err)
            break
        }

        frameMutex.Lock()
        latestFrame = frame
        frameMutex.Unlock()

        select {
        case frameUpdated <- struct{}{}:
        default:
        }
    }

    cmd.Process.Kill()
}

func readNextJPEG(reader *bufio.Reader) ([]byte, error) {
    startBytes, err := reader.ReadBytes(0xFF)
    if err != nil {
        return nil, err
    }
    b2, err := reader.ReadByte()
    if err != nil || b2 != 0xD8 {
        return nil, fmt.Errorf("invalid JPEG start")
    }
    jpeg := append(startBytes, b2)

    for {
        b, err := reader.ReadByte()
        if err != nil {
            return nil, err
        }
        jpeg = append(jpeg, b)
        l := len(jpeg)
        if l >= 2 && jpeg[l-2] == 0xFF && jpeg[l-1] == 0xD9 {
            break
        }
    }
    return jpeg, nil
}

func streamMJPEG(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")

    for {
        select {
        case <-r.Context().Done():
            return
        case <-frameUpdated:
            frameMutex.RLock()
            frame := latestFrame
            frameMutex.RUnlock()

            if frame == nil {
                continue
            }

            _, err := fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(frame))
            if err != nil {
                return
            }

            if _, err := w.Write(frame); err != nil {
                return
            }

            if _, err := fmt.Fprint(w, "\r\n"); err != nil {
                return
            }

            if f, ok := w.(http.Flusher); ok {
                f.Flush()
            }
        }
    }
}

func pageMain(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    fmt.Fprint(w, `
<!DOCTYPE html>
<html>
<head>
    <title>Arrow Key Control</title>
</head>
<body>
    <h1>Arrow Key Control Panel</h1>
    <p>Use WASD keys to move. Diagonal supported.</p>
    <pre id="result"></pre>
    <img src="/cam" style="width: 640px; margin-top: 20px; border: 1px solid #333;">
<script>
    let up_push = false;
    let down_push = false;
    let left_push = false;
    let right_push = false;
    let lastSent = "";

    document.addEventListener('keydown', (event) => {
        switch(event.key){
            case "w": up_push = true; break;
            case "s": down_push = true; break;
            case "a": left_push = true; break;
            case "d": right_push = true; break;
        }
    });

    document.addEventListener('keyup', (event) => {
        switch(event.key){
            case "w": up_push = false; break;
            case "s": down_push = false; break;
            case "a": left_push = false; break;
            case "d": right_push = false; break;
        }
    });

    function getDirection() {
        let dir = "";
        if (up_push) dir += "w";
        if (down_push) dir += "s";
        if (left_push) dir += "a";
        if (right_push) dir += "d";
        return dir;
    }

    function loop() {
        const dir = getDirection();
        if (dir !== lastSent) {
            lastSent = dir;
            if (dir === "") {
                fetch('/api/stop', { method: 'POST' })
                .then(res => res.json())
                .then(data => {
                    document.getElementById('result').textContent = JSON.stringify(data, null, 2);
                });
            } else {
                fetch('/api/move', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ direction: dir })
                })
                .then(res => res.json())
                .then(data => {
                    document.getElementById('result').textContent = JSON.stringify(data, null, 2);
                });
            }
        }
    }

    setInterval(loop, 100);
</script>
</body>
</html>
`)
}

func apiMove(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Only POST is allowed", http.StatusMethodNotAllowed)
        return
    }

    var req MoveRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, "Invalid JSON", http.StatusBadRequest)
        return
    }

    dir := req.Direction
    directionMap := map[string]string{
        "w":  "up",
        "a":  "left",
        "s":  "down",
        "d":  "right",
        "wa": "up-left",
        "wd": "up-right",
        "sa": "down-left",
        "sd": "down-right",
    }

    if val, ok := directionMap[dir]; ok {
        currentDirection = val
        isMoving = true
        fmt.Printf("Moving %s\n", val)
        json.NewEncoder(w).Encode(Message{Message: "Moving " + val})
    } else {
        http.Error(w, "Invalid direction (use w/a/s/d or combinations like wa)", http.StatusBadRequest)
    }
}

func apiStop(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Only POST is allowed", http.StatusMethodNotAllowed)
        return
    }

    isMoving = false
    currentDirection = ""
    fmt.Println("Stopped")
    json.NewEncoder(w).Encode(Message{Message: "Stopped"})
}

func main() {
    go captureLoop()

    http.HandleFunc("/", pageMain)
    http.HandleFunc("/cam", streamMJPEG)
    http.HandleFunc("/api/move", apiMove)
    http.HandleFunc("/api/stop", apiStop)

    log.Println("Server started at http://localhost:8080")
    log.Fatal(http.ListenAndServe(":8080", nil))
}
