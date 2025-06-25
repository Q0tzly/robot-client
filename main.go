package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type Message struct {
	Message string `json:"message"`
}

type MoveRequest struct {
	Direction string `json:"direction"`
}

var currentDirection = ""
var isMoving = false

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
			<p>Use Arrow keys to move. Diagonal supported.</p>
			<pre id="result"></pre>

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
	http.HandleFunc("/", pageMain)
	http.HandleFunc("/api/move", apiMove)
	http.HandleFunc("/api/stop", apiStop)

	fmt.Println("Server running at http://localhost:8080")
	http.ListenAndServe(":8080", nil)
}
