# llm-balancer

Kleiner, abhängigkeitsfreier Reverse-Proxy für lokale LLM-Server (llama.cpp, Ollama, vLLM, LM Studio …),
die nur **eine Anfrage gleichzeitig** sinnvoll verarbeiten können.

Statt zufällig zu verteilen (wie LiteLLM) zählt der Balancer die **offenen Verbindungen pro Server**:

* Ist ein Server frei → Anfrage geht sofort dorthin.
* Ist er belegt → nächster freier Server in der Liste.
* Sind alle belegt → Anfrage wartet in einer **FIFO-Warteschlange**, bis irgendein Slot frei wird.

Request und Response werden **1:1 durchgereicht** (Body, Header, SSE-Streaming ohne Pufferung).
Modellname & Co. werden nicht ausgewertet – nur zur Anzeige in der Web-UI gelesen.

```
Hermes-Agent ─► LiteLLM ─► llm-balancer :8080 ─┬─► LLM-Server A (1 Slot)
                                               ├─► LLM-Server B (1 Slot)
                                               └─► LLM-Server C (1 Slot)
```

## Funktionen

* Slot-Zählung pro Backend (`max_concurrent`, Standard 1). Der Slot wird erst freigegeben, wenn der Stream komplett beendet ist oder der Client abbricht
* FIFO-Queue mit optionalem Limit (`max_queue`) und Timeout (`queue_timeout_sec`)
* Priorität: Bei mehreren freien Servern wird der mit kleinerer `priority` bevorzugt (z. B. die schnellere GPU), sonst Round-Robin
* Failover: Verbindungsfehler oder HTTP 503 (z. B. llama.cpp „no slot available“ / „loading model“) → automatisch nächster Server; wiederholte Anfragen rücken an die Spitze der Queue
* Health-Checks (Standard `GET /v1/models` alle 15 s) für **freie** Server; ausgefallene Server werden übersprungen, bis sie wieder antworten
* `GET`-Anfragen (z. B. `/v1/models`, Health-Checks von LiteLLM) belegen keinen Slot und warten nie
* **Servergruppen mit Routing nach Modell**: Anfragen gehen nur an die Server, die das angefragte Modell bedienen – pro Gruppe mit eigener, fairer Warteschlange (ist die eine Gruppe ausgelastet, warten Anfragen für andere Gruppen nicht mit)
* Modellerkennung: die Modelle jedes Servers werden per `/v1/models` abgefragt; `GET /v1/models` am Balancer liefert die Modelle aller Server
* **Token-Zählung und Preise**: Input-/Output-Tokens jeder Anfrage aus der Antwort (OpenAI, Anthropic, Ollama, llama.cpp), Preis pro 1 Mio. Tokens je API-Token, Kosten pro Token und Tag
* Status-Übersicht pro Server: **beschäftigt**, **idle**, **fehlerhaft** (erreichbar, aber letzte Anfrage fehlgeschlagen – bis zur nächsten erfolgreichen, max. 15 min), **down** (Health-Check/Verbindung fehlgeschlagen), **deaktiviert**
* Statistik pro Tag und Server (7/30/90/365 Tage): Anfragen, Fehler, Abbrüche, Rechenzeit, Ø/Max-Dauer, Ø Zeit bis zum ersten Byte, Ø Wartezeit in der Queue, Datenmenge – als gestapelte Balkengrafik und Tabelle, CSV-Export; gespeichert in `/data/stats.json`
* Web-UI unter `/balancer/` mit Live-Status: Slots, laufende Anfragen (Prompt-Verarbeitung vs. Streaming, Dauer, Bytes), Warteschlange, letzte Anfragen, Ø Zeit bis zum ersten Byte / Ø Dauer, Server per Klick deaktivieren (drain) und aktivieren
* Server-Liste **im laufenden Betrieb** in der Web-UI ändern: hinzufügen, bearbeiten (URL, Name, Slots, Priorität), entfernen – ohne Neustart, laufende Anfragen laufen weiter
* Bearer-Token pro LLM-Server (z. B. für vLLM `--api-key` oder einen vorgeschalteten Proxy), in der UI hinterlegbar
* Zugriffsschutz für die LLM-API des Balancers mit eigenen Tokens (aus `API_KEYS` oder in der UI erzeugt, nur als Hash gespeichert)
* JSON-API `/balancer/api/status`, Prometheus-Metriken `/balancer/metrics`
* Eine statische Go-Binary ohne Abhängigkeiten, Docker-Image `FROM scratch` (~7 MB)

## Start

### docker compose / Portainer-Stack

```yaml
services:
  llm-balancer:
    image: ghcr.io/wakkomon/llm-balancer:latest   # oder: build: .
    restart: unless-stopped
    ports: ["8080:8080"]
    environment:
      BACKENDS: "gpu=http://192.168.1.50:8080,amd=http://192.168.1.60:8080@1"
    volumes:
      - llm-balancer-data:/data
volumes:
  llm-balancer-data:
```

Vollständige Vorlage: `portainer-stack.yml`.

Das Image wird von der GitHub-Action bei jedem Push auf `main` nach `ghcr.io/wakkomon/llm-balancer` gebaut
(beim ersten Mal das Package unter GitHub → Packages auf „public“ stellen oder `docker login ghcr.io`).

### Ohne Docker

```bash
go build -o llm-balancer .
BACKENDS="http://host-a:8080,http://host-b:8080" ./llm-balancer
```

## Konfiguration

### Server-Liste: Startwert und Änderungen in der UI

`BACKENDS` (bzw. `backends` in `config.json`) ist nur die **Startkonfiguration**. Sobald du die Liste in der
Web-UI änderst, wird sie in `DATA_DIR/backends.json` gespeichert (im Container `/data/backends.json`) und hat ab
dann Vorrang – auch nach Neustarts und Image-Updates, solange `/data` auf einem Volume liegt.
„Auf Startkonfiguration zurücksetzen“ in der UI löscht die Datei und lädt wieder `BACKENDS`.

Änderungen wirken sofort:

* Neuer Server oder mehr Slots → wartende Anfragen aus der Queue starten sofort dort.
* Geänderte URL → sofortiger Health-Check; laufende Anfragen auf der alten URL laufen zu Ende.
* Entfernter Server → bekommt keine neuen Anfragen mehr, laufende werden normal beendet.

Mit `ADMIN_TOKEN` lassen sich Änderungen absichern; die UI fragt dann einmal nach dem Token und merkt ihn sich im Browser.
Ohne Token kann jeder, der die UI erreicht, die Server-Liste ändern.

Für einen Bind-Mount statt eines Docker-Volumes muss der Ordner dem User 65534 gehören
(`chown 65534:65534 ./data`), sonst kann der Container nicht speichern – die UI zeigt das dann als Warnung an.

### Servergruppen und Modelle

Eine Gruppe ist eine Menge von Servern plus die Modelle, die sie bedienen. Ein Server gehört zu höchstens einer Gruppe
(Dialog „Bearbeiten“ des Servers oder Dialog der Gruppe). Die Zuordnung einer Anfrage passiert über das Feld `model`:

1. Gruppen, deren Modellliste passt – eingetragene Namen oder Muster mit `*` (z. B. `qwen3*`), und, wenn
   „zusätzlich gemeldete Modelle“ aktiv ist (Standard), alle Modelle, die ihre Server über `/v1/models` melden.
   Passen mehrere Gruppen, zählen die Server aller passenden Gruppen.
2. Sonst: alle Server, die das Modell selbst melden (auch ohne Gruppe).
3. Sonst: alle Server – oder 404, wenn `UNKNOWN_MODEL=reject` gesetzt ist.

Groß-/Kleinschreibung spielt keine Rolle. Die Modelle der Server werden beim Health-Check (alle 15 s, nur bei freien Servern)
neu abgefragt. Im Feld **Modell** über der Serverliste lässt sich ein Modell auswählen: die UI zeigt dann, welche Gruppe
und welche Server es bekommen, und blendet die übrigen Server aus. `GET /balancer/api/route?model=…` liefert dasselbe als JSON.

Ohne Gruppen verhält sich der Balancer wie bisher (alle Server bedienen alle Modelle).

### Token-Zählung und Preise

Der Balancer liest die Token-Angaben aus den Antworten: `usage` (OpenAI, auch im letzten Stream-Chunk), `input_tokens` /
`output_tokens` (Anthropic), `prompt_eval_count` / `eval_count` (Ollama nativ) und `timings` (llama.cpp). Damit
OpenAI-kompatible Server auch bei Streams Zahlen liefern, ergänzt der Balancer Stream-Anfragen an `…/completions` um
`"stream_options": {"include_usage": true}`, sofern der Client das nicht selbst setzt (abschaltbar mit `STREAM_USAGE=false`;
der Client erhält dann einen zusätzlichen letzten Chunk mit `usage` und leerem `choices`).

Pro API-Token lässt sich unter **API-Zugriff → Preis** ein Preis pro 1 Mio. Input- und Output-Tokens hinterlegen
(Währung über `CURRENCY`). Die Kosten werden beim Abschluss jeder Anfrage mit dem dann gültigen Preis berechnet; eine
Preisänderung rechnet bereits gezählte Kosten nicht um. Die Statistik zeigt Tokens pro Server und Tag sowie Anfragen,
Tokens und Kosten pro API-Token im gewählten Zeitraum; „Letzte Anfragen“ zeigt Tokens, Tokens/s und Kosten jeder Anfrage
(die letzten 300, davon 15 sichtbar, der Rest per Scrollen).

### Tokens

**Zugriff auf den Balancer (Clients → Balancer).** Sobald mindestens ein Token existiert, brauchen alle Anfragen an die
LLM-API (alles außerhalb von `/balancer/`) einen gültigen Token als `Authorization: Bearer <token>`, `x-api-key` oder `api-key`.
Sonst antwortet der Balancer mit 401 im OpenAI-Fehlerformat. Tokens kommen aus `API_KEYS` (fest, nur per Umgebungsvariable
änderbar) oder werden in der UI unter **API-Zugriff** erzeugt. Ein erzeugter Token wird genau einmal angezeigt; gespeichert
wird nur sein SHA-256-Hash in `/data/access.json`. Die UI zeigt pro Token „zuletzt benutzt“ und die Anzahl Anfragen, in
„Letzte Anfragen“ steht der Token-Name als Client. Ohne jeden Token ist die API offen (die UI weist darauf hin).

**Tokens der LLM-Server (Balancer → Server).** Pro Server kann ein Bearer-Token hinterlegt werden (Dialog „Bearbeiten“
oder `BACKEND_KEYS="gpu=sk-…"` als Startwert). Er wird als `Authorization: Bearer …` gesendet, auch beim Health-Check.
Die Zugangsdaten des Clients werden nie an die Server weitergegeben, sobald der Balancer selbst Tokens prüft oder der Server
einen eigenen Token hat. Lehnt ein Server den Token ab (401/403), wird er als *fehlerhaft* bzw. beim Health-Check als *down*
mit entsprechendem Hinweis angezeigt. Server-Tokens verlassen den Balancer nie über API oder UI und liegen in
`/data/backends.json` (Dateirechte 0600).

### Alle Optionen

Entweder per Umgebungsvariablen oder per `config.json` (Pfad über `CONFIG`, sonst `./config.json` bzw.
`/config/config.json`). Umgebungsvariablen überschreiben die Datei. Siehe `config.example.json`.

| Variable / Feld | Standard | Bedeutung |
|---|---|---|
| `BACKENDS` / `backends` | – | Startkonfiguration: `name=url*maxParallel@priorität`, kommagetrennt. Name, `*N` und `@N` sind optional. Darf leer sein, wenn alles in der UI eingetragen wird |
| `DATA_DIR` / `data_dir` | `data` (Docker: `/data`) | Ablage für die in der UI geänderte Server-Liste |
| `STATS_DAYS` / `stats_days` | `400` | Aufbewahrung der Tagesstatistik |
| `TZ` | UTC | Zeitzone für die Tagesgrenzen der Statistik, z. B. `Europe/Berlin` |
| `API_KEYS` / `api_keys` | – | Tokens für Clients der LLM-API: `name=token,…` (oder nur `token,…`) |
| `BACKEND_KEYS` | – | Startwerte für die Bearer-Tokens der Server: `name=token,…` |
| `UNKNOWN_MODEL` / `unknown_model` | `all` | `reject`: Modelle, die keine Gruppe und kein Server kennt, mit 404 ablehnen (nur wenn Gruppen existieren) |
| `STREAM_USAGE` / `stream_usage` | `true` | Stream-Anfragen um `stream_options.include_usage` ergänzen, damit Tokens gezählt werden |
| `CURRENCY` / `currency` | `€` | Währung der Preise |
| `ADMIN_TOKEN` / `admin_token` | – | Wenn gesetzt, sind Änderungen in UI/API nur mit diesem Token möglich (Header `X-Admin-Token`) |
| `LISTEN` / `listen` | `:8080` | Listen-Adresse |
| `QUEUE_TIMEOUT_SEC` / `queue_timeout_sec` | `900` | Max. Wartezeit in der Queue (0 = unbegrenzt), danach HTTP 504 |
| `MAX_QUEUE` / `max_queue` | `0` | Max. Länge der Queue (0 = unbegrenzt), danach HTTP 503 |
| `HEALTH_PATH` / `health_path` | `/v1/models` | Pfad für Health-Checks; leer = deaktiviert (dann 10 s Sperre nach Verbindungsfehler). Für llama.cpp passt auch `/health` |
| `HEALTH_INTERVAL_SEC` / `health_interval_sec` | `15` | Intervall der Health-Checks |
| `slot_methods` | `["POST"]` | HTTP-Methoden, die einen Slot belegen |
| `retry_status` | `[503]` | Upstream-Statuscodes, bei denen auf einen anderen Server ausgewichen wird |
| `ui_prefix` | `/balancer` | Pfad der Web-UI/API (alles andere wird durchgereicht) |

Beispiel mit einem Server, der 2 parallele Anfragen kann (z. B. llama.cpp mit `-np 2`), und Bevorzugung der GPU:

```
BACKENDS="gpu=http://10.0.0.5:8080*2@0,amd=http://10.0.0.6:8080@1"
```

## LiteLLM anbinden

Statt mehrerer Deployments mit Zufallsverteilung nur noch **ein** Deployment, das auf den Balancer zeigt:

```yaml
model_list:
  - model_name: local-llm
    litellm_params:
      model: openai/qwen3          # Modellname wird 1:1 an den Backend-Server weitergegeben
      api_base: http://llm-balancer:8080/v1
      api_key: "llmb-…"            # Token aus API_KEYS bzw. „API-Zugriff“ in der UI ("none", wenn keiner eingerichtet ist)
      timeout: 1800                # Queue-Wartezeit + Generierungsdauer einplanen
      stream_timeout: 1800
```

Hinweise:

* Der Modellname wird nicht umgeschrieben. Die Backends müssen den Namen also entweder alle kennen
  (Ollama: gleiches Modell-Tag) oder ihn ignorieren (llama.cpp-Server mit einem geladenen Modell).
* LiteLLM-Timeouts und Retries so wählen, dass Anfragen nicht abgebrochen werden, während sie in der Queue warten –
  sonst schickt LiteLLM Duplikate hinterher. Ein vom Client abgebrochener Request wird sauber aus der Queue entfernt
  bzw. sein Slot sofort freigegeben.
* Jede Antwort enthält den Header `X-LLM-Balancer-Backend` mit dem verwendeten Server.

## Web-UI

`http://<host>:8080/balancer/` – aktualisiert sich jede Sekunde. Status pro Server: *frei*, *belegt*,
*nicht erreichbar*, *deaktiviert*. Pro Server: **Bearbeiten**, **Deaktivieren (drain)** – nimmt ihn aus der
Verteilung, laufende Anfragen laufen zu Ende, praktisch für Modellwechsel oder Wartung – und **Entfernen**.
Oben rechts: **+ Server hinzufügen**.

Die Status-Ansicht ist ohne Anmeldung sichtbar; für Änderungen `ADMIN_TOKEN` setzen.

### API

| Methode | Pfad | |
|---|---|---|
| GET | `/balancer/api/status` | kompletter Status |
| GET | `/balancer/api/backends` | aktuelle Server-Liste |
| POST | `/balancer/api/backends` | Server hinzufügen, Body `{"name","url","max_concurrent","priority","enabled","api_key"}` |
| PUT | `/balancer/api/backends/{name}` | Server ändern (gleicher Body, Umbenennen erlaubt; `api_key` weglassen = behalten, `""` = entfernen) |
| DELETE | `/balancer/api/backends/{name}` | Server entfernen |
| POST | `/balancer/api/backends/{name}/enable` bzw. `/disable` | aktivieren / drain |
| POST | `/balancer/api/reset` | zurück zur Startkonfiguration |
| GET/POST | `/balancer/api/tokens` | Client-Tokens auflisten / erzeugen (`{"name","token"?}`) |
| PUT | `/balancer/api/tokens/{name}` | Preis setzen: `{"price_in":0.4,"price_out":1.6}` (pro 1 Mio. Tokens) |
| DELETE | `/balancer/api/tokens/{name}` | Client-Token löschen |
| POST | `/balancer/api/groups` | Gruppe anlegen: `{"name","models":[…],"auto_models","members":[…]}` |
| PUT / DELETE | `/balancer/api/groups/{name}` | Gruppe ändern (umbenennen erlaubt; `members` weglassen = unverändert) / löschen |
| GET | `/balancer/api/route?model=…` | Welche Gruppe/Server bekämen dieses Modell? |
| GET | `/balancer/api/recent?after=<seq>` | Letzte Anfragen (max. 300), nur neuere als `seq` |
| GET | `/balancer/api/stats?days=30` | Tagesstatistik (`&format=csv` für CSV) |
| POST | `/balancer/api/stats/reset` | Statistik löschen |

## Entwicklung

```bash
go test -race ./...
```

Die Tests simulieren langsame Ein-Slot-Server und prüfen u. a. Verteilung, Queue, ungepuffertes Streaming,
Failover bei toten Servern und bei 503, Abbruch in der Queue ohne Slot-Leck sowie Drain.
