pi@raspberrypi:~/src $ cat main.go
package main

import (
    "bufio"
    "fmt"
    "log"
    "net/http"
    "os"
    "os/exec"
    "sync"
    "syscall"
    "time"
    "unsafe"
)

//////////////////////////////
// Camera
//////////////////////////////

var (
    latestFrame []byte
    frameMutex  sync.RWMutex
)

func captureLoop() {
    cmd := exec.Command("bash", "-c", `
        libcamera-vid -t 0 --codec mjpeg --width 1280 --height 720 --framerate 24 -o -
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
        default:
            frameMutex.RLock()
            frame := latestFrame
            frameMutex.RUnlock()
            if frame == nil {
                time.Sleep(10 * time.Millisecond)
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
            time.Sleep(40 * time.Millisecond)
        }
    }
}

//////////////////////////////
// Buzzer
//////////////////////////////

const (
    gpioGetLineHandleIoctl  = 0xC16CB403
    gpioHandleSetLineValues = 0xC040B409
    gpioHandleRequestOutput = 0x2
    GPIOHANDLES_MAX         = 64
)

type gpioHandleRequest struct {
    LineOffsets   [GPIOHANDLES_MAX]uint32
    Flags         uint32
    DefaultValues [GPIOHANDLES_MAX]uint8
    ConsumerLabel [32]byte
    Lines         uint32
    Fd            int32
}

type gpioHandleData struct {
    Values [GPIOHANDLES_MAX]uint8
}

var (
    buzzerFd    int32
    buzzerMutex sync.Mutex
    buzzerOn    bool
)

func initBuzzer(line uint32) {
    chip := "/dev/gpiochip0"
    f, err := os.OpenFile(chip, os.O_RDWR, 0)
    if err != nil {
        log.Fatal(err)
    }
    defer f.Close()

    var req gpioHandleRequest
    req.LineOffsets[0] = line
    req.Flags = gpioHandleRequestOutput
    req.DefaultValues[0] = 0
    req.Lines = 1

    _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), gpioGetLineHandleIoctl, uintptr(unsafe.Pointer(&req)))
    if errno != 0 {
        log.Fatal(errno)
    }
    buzzerFd = req.Fd
}

func setBuzzer(value uint8) {
    var data gpioHandleData
    data.Values[0] = value
    _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(buzzerFd), gpioHandleSetLineValues, uintptr(unsafe.Pointer(&data)))
    if errno != 0 {
        log.Println("ioctl error:", errno)
    }
}

func buzzerToggle() {
    buzzerMutex.Lock()
    defer buzzerMutex.Unlock()

    if buzzerOn {
        // OFF
        buzzerOn = false
        setBuzzer(0)
        return
    }

    // ON
    buzzerOn = true
    go func() {
        frequency := 220
        duty := 0.5
        period := time.Second / time.Duration(frequency)
        highTime := time.Duration(float64(period) * duty)
        lowTime := period - highTime

        for {
            buzzerMutex.Lock()
            on := buzzerOn
            buzzerMutex.Unlock()
            if !on {
                setBuzzer(0)
                return
            }
            setBuzzer(1)
            time.Sleep(highTime)
            setBuzzer(0)
            time.Sleep(lowTime)
        }
    }()
}

//////////////////////////////
// Flame sensor
//////////////////////////////

const (
    GPIOHANDLE_REQUEST_INPUT           = 0x1
    GPIOHANDLE_GET_LINE_VALUES_IOCTL   = 0xc040b408
)

type gpiohandleRequestInput struct {
    LineOffsets   [64]uint32
    Flags         uint32
    DefaultValues [64]uint8
    ConsumerLabel [32]byte
    Lines         uint32
    Fd            int32
}

var (
    flameFd       int
    flameDetected bool
    flameMutex    sync.RWMutex
)

func initFlameSensor(pin uint32) {
    chip := "/dev/gpiochip0"
    fd, err := syscall.Open(chip, syscall.O_RDONLY, 0)
    if err != nil {
        log.Fatal(err)
    }

    var req gpiohandleRequestInput
    req.LineOffsets[0] = pin
    req.Flags = GPIOHANDLE_REQUEST_INPUT
    req.Lines = 1
    copy(req.ConsumerLabel[:], []byte("flame-sensor"))

    _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0xc16cb403, uintptr(unsafe.Pointer(&req)))
    if errno != 0 {
        log.Fatal(errno)
    }
    flameFd = int(req.Fd)

    go monitorFlame()
}

func monitorFlame() {
    for {
        var data struct{ Values [64]uint8 }
        _, _, errno := syscall.Syscall(uintptr(syscall.SYS_IOCTL), uintptr(flameFd), uintptr(GPIOHANDLE_GET_LINE_VALUES_IOCTL), uintptr(unsafe.Pointer(&data)))
        if errno != 0 {
            log.Println("Failed reading flame sensor:", errno)
        } else {
            flameMutex.Lock()
            //flameDetected = data.Values[0] == 0 // LOW = flame
            flameDetected = data.Values[0] == 1 // HIGH = flame
            flameMutex.Unlock()
        }
        time.Sleep(200 * time.Millisecond)
    }
}

func flameStatusHandler(w http.ResponseWriter, r *http.Request) {
    flameMutex.RLock()
    detected := flameDetected
    flameMutex.RUnlock()
    if detected {
        fmt.Fprint(w, "1")
    } else {
        fmt.Fprint(w, "0")
    }
}

//////////////////////////////
// HTTP Handlers
//////////////////////////////

func servePage(w http.ResponseWriter, r *http.Request) {
    html := `<!DOCTYPE html>
<html>
<head>
<title>Robot Console</title>
<style>
body {
    font-family: Arial, sans-serif;
    background: #f0f2f5;
    text-align: center;
    padding: 20px;
}
h1 { color: #333; }

#cam {
    border: 2px solid #ccc;
    border-radius: 8px;
    width: 640px;
    height: 360px;
}

.button {
    padding: 15px 30px;
    font-size: 18px;
    margin: 20px;
    border: none;
    border-radius: 8px;
    cursor: pointer;
    transition: 0.3s;
}

#buzzerBtn { background-color: #007bff; color: white; }
#buzzerBtn.active { background-color: #dc3545; }

#flameStatus {
    display: inline-block;
    width: 30px;
    height: 30px;
    border-radius: 50%;
    margin-left: 10px;
    vertical-align: middle;
}

.card {
    display: inline-block;
    background: white;
    padding: 20px;
    border-radius: 10px;
    box-shadow: 0 4px 8px rgba(0,0,0,0.1);
    margin: 10px;
}
</style>
</head>
<body>
<h1>Robot Console</h1>

<div class="card">
    <h2>Camera</h2>
    <img id="cam" src="/cam">
</div>

<div class="card">
    <h2>Buzzer</h2>
    <button id="buzzerBtn" class="button" onclick="toggleBuzzer()">ON/OFF</button>
</div>

<div class="card">
    <h2>Flame Sensor</h2>
    <span id="flameStatus"></span>
    <span id="flameText"></span>
</div>

<script>
function toggleBuzzer() {
    fetch('/buzzer/toggle').then(()=>{
        let btn = document.getElementById('buzzerBtn');
        btn.classList.toggle('active');
    });
}

function updateFlame() {
    fetch('/flame/status')
    .then(r=>r.text())
    .then(v=>{
        let status = document.getElementById('flameStatus');
        let text = document.getElementById('flameText');
        if(v==='1') {
            status.style.background='red';
            text.textContent=' Flame Detected!';
        } else {
            status.style.background='green';
            text.textContent=' No Flame';
        }
    });
}

updateFlame();
setInterval(updateFlame, 500);
</script>
</body>
</html>`
    w.Header().Set("Content-Type", "text/html")
    fmt.Fprint(w, html)
}

//////////////////////////////
// Main
//////////////////////////////

func main() {
    go captureLoop()
    initBuzzer(18)
    initFlameSensor(17)

    http.HandleFunc("/", servePage)
    http.HandleFunc("/cam", streamMJPEG)
    http.HandleFunc("/buzzer/toggle", func(w http.ResponseWriter, r *http.Request) {
        buzzerToggle()
        fmt.Fprint(w, "Toggled")
    })
    http.HandleFunc("/flame/status", flameStatusHandler)

    log.Println("Server started at http://localhost:8080")
    log.Fatal(http.ListenAndServe(":8080", nil))
}
