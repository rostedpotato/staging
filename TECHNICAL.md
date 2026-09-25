# Technical Reference — Staging Self-Service Platform

Acuan teknis untuk siapa pun yang akan mengembangkan lebih lanjut. Untuk
panduan pemakaian end-user, lihat `../USER_GUIDE.md`. Untuk ringkasan fitur +
runbook operasional, lihat `README.md`.

---

## 1. Arsitektur & filosofi

- **Satu binary Go (stdlib-heavy) + SQLite embedded (pure-Go, no CGO) +
  server-rendered `html/template`.** Tidak ada Node build, tidak ada DB
  server terpisah, tidak ada frontend framework. Alasan: host staging sudah
  sangat sibuk (load ~9.6, RAM nyaris penuh saat platform ini dibuat) —
  stack harus seringan mungkin.
- **Read-only terhadap staging** untuk semua hal selain deploy yang memang
  disengaja lewat Jenkins. Discovery (`internal/discovery`) tidak pernah
  menulis ke `/data/app`, nginx config, atau memanggil `docker` selain `ps`.
- **Semua state aplikasi (users, sessions, bookings, deployments, audit) ada
  di satu file SQLite** (`data/platform.db`), di-mount sebagai volume Docker
  agar persist antar restart/redeploy container.
- Container platform berjalan **terpisah** dari container aplikasi staging
  (watersheep/fisherman punya Docker network sendiri) — implikasinya dibahas
  di §5 (runtime version check) dan §7 (mounts).

## 2. Struktur kode

```
main.go                        entrypoint: load config, wire semua service, start HTTP server
internal/
  config/       config.go      load config.json + default values
  store/                       lapisan SQLite (satu *sql.DB, MaxOpenConns=1)
    schema.sql                 skema DDL, di-embed & dijalankan idempoten saat startup
    store.go                   Open()/Close(), helper nowUTC()
    users.go                   CRUD user, bcrypt, temp password
    sessions.go                cookie session token
    reservations.go            booking per-slot per-hari, cek overlap transaksional
    deployments.go             riwayat deploy, log baris per baris, slot lock
    slots.go                   slot_settings (hidden/show)
    audit.go                   audit_logs, search
  auth/                        login, RBAC, middleware
    auth.go                    Authenticator, LoadUser/RequireUser/RequireAdmin, session cookie
    handlers.go                POST /auth/login, GET/POST /reset-password, /logout
    render.go                  render halaman login/reset-password
  discovery/                   scan read-only ke host staging
    discovery.go                Scan(): orkestrasi utama, gabungkan semua sumber
    docker.go                   exec `docker ps`, parse JSON
    gitinfo.go                  exec `git` per slot repo (branch/commit/waktu)
    nginx.go                    parse nginx sites-enabled -> domain per slot
    runtime_version.go          GET /actuator/info ke watersheep/fisherman yang running
    model.go                    tipe Slot/Service + Slot.Overall() (health badge)
  jenkins/      jenkins.go     client Jenkins: Trigger/ResolveBuild/BuildStatus/ConsoleText
  deploy/       deploy.go      orkestrasi deploy: cek booking, slot lock, state machine, watcher goroutine
  web/                         HTTP handlers + template
    web.go                     Server struct, routing, cache scan background, dashboard handler
    bookings.go                handler booking
    deploy_handlers.go         handler deploy + SSE log stream
    admin_handlers.go          handler admin (users/slots)
    templates/*.html           html/template, di-embed via go:embed
    static/style.css           satu file CSS, di-embed
```

## 3. Alur request penting

### 3.1 Auth
- `POST /auth/login` → `store.VerifyPassword` (bcrypt) → set cookie session
  (`internal/auth/auth.go:setSession`) → kalau `MustResetPassword`, redirect
  paksa ke `/reset-password`.
- `LoadUser` middleware (dibungkus di lapisan terluar `Handler()`) menaruh
  `*store.User` ke context di **setiap** request (termasuk publik) supaya
  handler downstream bisa `auth.UserFrom(ctx)`.
- `RequireUser`/`RequireAdmin` adalah middleware terpisah yang dipasang
  per-route di `web.go`.
- **Tidak ada pembuatan akun lewat login.** Akun hanya dibuat oleh admin
  (`/admin/users/create`) atau oleh `bootstrapAdmin()` di `main.go` sekali
  saat `CountUsers() == 0`.

### 3.2 Booking
- `store.CreateReservation` (`reservations.go`) melakukan cek overlap +
  insert dalam **satu transaksi SQL** — mencegah race condition dua booking
  bentrok di slot+tanggal yang sama.
- Booking selalu whole-day (00:00:00–23:59:59 lokal), lihat
  `bookings.go:parseBookingDate`.
- `ExpireDue()` dipanggil lazy di awal `ListActive()`/`ActiveForSlot()` untuk
  menandai booking yang lewat tanggal sebagai `expired` — tidak ada cron
  terpisah untuk ini.

### 3.3 Deploy
- `deploy.Service.Start()` (`internal/deploy/deploy.go`):
  1. Validasi service dikenal, branch **XOR** tag (`ErrTagNotAllow`,
     `ErrBadRef`).
  2. `checkBooking()` — admin & `!RequireBooking` bisa bypass syarat booking,
     TAPI tetap ditolak kalau slot sedang dibooking **orang lain**
     (`ErrBookedOther`). User biasa wajib punya booking aktif untuk slot itu
     sendiri (`ErrNotBooked`).
  3. `store.CreateDeployment` — mengunci slot (tabel `slot_locks`); kalau
     slot masih ada deployment aktif, ditolak (`ErrSlotBusy`, lihat
     `store/deployments.go`).
  4. `jenkins.Trigger()` → dapat queue URL dari header `Location`.
  5. Goroutine `watch()` di background: poll queue → resolve build number →
     poll status sambil menyimpan baris log baru ke `deployment_logs` →
     tandai `success`/`failed`/`error` saat build selesai (deadline 60 menit).
- `handleDeployStream` (SSE) di `deploy_handlers.go` membaca
  `deployment_logs` tiap 1.5 detik dan mem-flush baris baru ke client;
  berhenti otomatis begitu status deployment tidak lagi aktif.
- **Rollback**: `deploy.Service.Rollback()` mencari
  `PreviousSuccess(slot, service, excludeID)` lalu memanggil `Start()` lagi
  dengan ref yang sama. Endpoint `/deploy/rollback` dijaga admin-only di
  level route (`RequireUser`) + cek eksplisit `u.IsAdmin()` di handler.

### 3.4 Discovery (dashboard)
Lihat §4 untuk detail lengkap alur scan + caching.

## 4. Discovery & caching — bagian paling sering berubah

Ini bagian yang paling banyak diperbaiki selama pengembangan, jadi paling
penting dipahami sebelum menyentuhnya lagi.

### 4.1 Kenapa background scan, bukan per-request
Dulu `Scan()` dipanggil sinkron di setiap request (`s.scan(ctx)` di
handler). Masalahnya: `Scan()` menjalankan `docker ps` + `git rev-parse` ×3
**per slot** — di host yang sibuk ini bisa memakan puluhan detik, jadi user
yang kliknya kebetulan pas cache expired harus menunggu.

**Solusi final** (`internal/web/web.go`):
- `Server.StartBackgroundScan(ctx)` dipanggil sekali dari `main.go`, jalan
  sinkron sekali di awal (supaya load pertama tidak kosong), lalu jalan di
  goroutine terpisah dengan `time.Ticker` setiap `scanCacheTTL` (5 menit).
- Semua handler HTTP **hanya baca cache** lewat `s.scan()`/`s.rawScan()` —
  **tidak pernah** memicu scan sendiri. Ini menghilangkan lag klik sepenuhnya.
- `Server.rawScan{Mu,At,Slots,Err}` adalah cache in-memory, dilindungi
  `sync.RWMutex`.

### 4.2 Kenapa error scan tidak boleh menghapus data lama
`discovery.Scan()` mengembalikan **slot list DAN error sekaligus** — slot
list dari `os.ReadDir` (disk, selalu murah & andal), error biasanya cuma dari
`docker ps` yang gagal/timeout. Kalau `refreshScan` naif "buang semua kalau
ada error", maka satu `docker ps` timeout (sangat mungkin di host sibuk)
akan membuat dashboard kosong total.

**Aturan yang dipakai sekarang** (`refreshScan` di `web.go`):
```go
if len(slots) > 0 {
    s.rawSlots = slots   // simpan meski err != nil
}
s.rawErr = err            // error selalu dicatat, untuk banner
```
Banner "Discovery warning" di dashboard (`handleDashboard`) HANYA muncul
kalau `err != nil` **DAN** `len(views) == 0` — kalau ada data yang bisa
ditampilkan (walau sebagian dari cache lama), jangan tampilkan warning yang
menakut-nakuti user padahal datanya valid.

### 4.3 Kenapa `docker ps` sempat gagal terus (`signal: killed`)
- Awalnya timeout `context.WithTimeout` untuk `docker ps` di
  `internal/discovery/docker.go` cuma 10 detik — di host dengan ~40
  container, itu kurang. Dinaikkan bertahap 10s → 30s → **60s** (aman karena
  scan sekarang di background, tidak menahan user).
- Juga dihapus flag `--no-trunc` (tidak perlu, kita cuma baca
  Names/Image/State/Status/Ports; flag itu cuma memperbesar output karena
  ikut menampilkan label panjang).

### 4.4 `docker.sock` harus read-write, bukan `:ro`
Di `docker-compose.yml`, mount `/var/run/docker.sock` **jangan** diberi
suffix `:ro`. Koneksi ke Unix domain socket butuh akses tulis pada file
socket itu sendiri walau perintahnya read-only (`docker ps`). `:ro` bikin
`docker` CLI di dalam container gagal connect sama sekali dengan
`permission denied` untuk SEMUA command.

### 4.5 Git checkout vs `.env` sebagai sumber commit/branch
`internal/discovery/gitinfo.go`:
- **Git checkout aktual di disk adalah sumber kebenaran** untuk
  Branch/Commit/Version yang tampil di dashboard — bukan `.env`
  (`VITE_GIT_*`), karena `.env` cuma mencatat versi saat **build frontend
  terakhir** dan sering basi (beda dari kondisi git checkout sekarang).
- `.env` (`envGitInfo`) hanya dipakai sebagai **fallback** kalau git gagal
  memberi info sama sekali.
- Version label dibentuk dari `branch + "-" + commit` (bukan
  `VITE_GIT_FULL_VERSION`), supaya selalu konsisten dengan apa yang ada di
  disk.
- `runGit()` **wajib** memakai `-c safe.directory=*` — tanpa ini, git modern
  menolak baca repo yang dimiliki user lain (`detected dubious ownership`),
  yang terjadi di sini karena repo di host dimiliki `root` sementara proses
  di container jalan sebagai user lain. Ini pernah jadi penyebab SEMUA slot
  menampilkan commit yang sama (fallback ke `.env` yang kebetulan sama untuk
  banyak slot).

### 4.6 WBO bukan container Docker
WBO (Web Backoffice) adalah frontend Vite yang di-build ke
`<repoDir>/dist` dan disajikan langsung oleh nginx — **bukan** container
seperti watersheep/fisherman. Direpresentasikan sebagai `Service` semu
(`wboService()` di `discovery.go`):
- `Found`/`State` ditentukan dari keberadaan `dist/index.html` di disk.
- `RunningVersion` dibaca dari `dist/details.json`
  (`{"version": "v1.18.1-b9f5776d", ...}`) — file ini di-generate oleh build
  frontend, sudah ada di disk yang sama, jadi dibaca langsung tanpa HTTP call.

### 4.7 Versi runtime watersheep/fisherman via Actuator
`internal/discovery/runtime_version.go`:
- Untuk service yang statusnya `running` dan punya `HostPort`, GET
  `http://host.docker.internal:<hostPort>/actuator/info`, ambil
  `build.version`. Timeout 2 detik, gagal senyap (best-effort — kalau gagal,
  field cuma kosong, tidak mengganggu scan lain).
- **`host.docker.internal` butuh `extra_hosts: host-gateway`** di
  `docker-compose.yml` — tidak otomatis di Docker Linux (beda dari Docker
  Desktop macOS/Windows).
- Dipanggil **concurrent** (`fetchRunningVersions`, goroutine per service)
  supaya tidak jadi N×services HTTP round-trip berurutan.
- Field `Service.RunningVersion` dipakai bersama oleh watersheep/fisherman
  (dari actuator) dan WBO (dari `details.json`) — sengaja disatukan supaya
  template dashboard tidak perlu tahu bedanya.

## 5. Ketahanan (resilience) di beban banyak user

- **`recoverPanic` middleware** (`web.go`, lapisan terluar `Handler()`):
  panic di satu request cuma jadi 500 untuk request itu, tidak menjatuhkan
  seluruh proses Go (yang akan mematikan server untuk SEMUA user).
- **HTTP server timeouts** (`main.go`): `ReadHeaderTimeout`, `ReadTimeout`,
  `IdleTimeout` diset untuk mencegah koneksi lambat/slowloris menumpuk. **Tidak
  ada `WriteTimeout` global** karena akan memutus SSE deploy-log stream yang
  memang sengaja long-lived.
- **SQLite**: `db.SetMaxOpenConns(1)` + `busy_timeout(5000)` + WAL. Semua
  query (termasuk read) diserialisasi lewat 1 koneksi — pilihan aman untuk
  menghindari "database is locked", dan untuk beban puluhan user query-nya
  cukup ringan (milidetik) sehingga tidak jadi bottleneck nyata. **Jangan**
  naikkan `MaxOpenConns` tanpa alasan kuat — SQLite writer tetap satu.
- **Booking overlap check**: transaksi SQL tunggal (§3.2) mencegah race
  condition, bukan lock aplikasi.
- Discovery scan sepenuhnya lepas dari request path (§4.1) — beban N user
  klik dashboard bersamaan = N kali baca cache in-memory, bukan N kali scan.

## 6. Konfigurasi (`config.json`)

Lihat `config.example.json` untuk template lengkap. Poin yang sering
terlewat:

| Key | Catatan |
|---|---|
| `discovery.refreshSeconds` | interval `<meta refresh>` di dashboard (reload halaman browser), default 900 (15 menit) |
| `scanCacheTTL` (konstanta di `web.go`, BUKAN di config) | interval scan discovery background, saat ini 5 menit — sengaja tidak di-expose ke config.json, ubah langsung di kode kalau perlu |
| `deploy.requireBooking` | kalau `true`, user non-admin wajib punya booking aktif di slot yang sama untuk bisa deploy |
| `deploy.jenkinsToken` | SECRET, shared untuk semua job Jenkins (bukan per-service) |
| `auth.secure` | wajib `true` kalau sudah di belakang HTTPS (cookie `Secure` flag) |
| `hiddenSlots` | daftar slot yang disembunyikan lewat config (terpisah dari `slot_settings` di DB yang diatur admin via UI `/admin/slots`) — keduanya di-OR-kan (`Config.Hidden()` + `store.HiddenSlots()`) |

## 7. Docker & mounts (`docker-compose.yml`)

| Mount | Mode | Kenapa |
|---|---|---|
| `./config.json` | `:ro` | config aplikasi |
| `./data` | rw | SQLite DB, harus persist antar restart |
| `/var/run/docker.sock` | **rw (bukan `:ro`)** | lihat §4.4 — read-write wajib walau kita cuma pakai `docker ps` |
| `/data/app` | `:ro` | slot dirs, git repos, `.env`, `dist/` |
| `/etc/nginx/sites-enabled` | `:ro` | mapping domain per slot |
| `extra_hosts: host.docker.internal:host-gateway` | — | wajib di Linux, dipakai `runtime_version.go` untuk menjangkau container lain |
| `cpus: 0.5`, `mem_limit: 192m` | — | sengaja kecil, host staging sudah sibuk. Kalau nanti scan/HTTP checks makin berat, ini kandidat pertama untuk dinaikkan |

## 8. Testing

- `internal/store`, `internal/deploy`, `internal/web` punya test suite
  (`go test ./...`). `internal/discovery` **belum** punya test otomatis
  (semua perbaikannya sejauh ini diverifikasi manual di server staging
  sungguhan karena tergantung `docker`/`git` binary + filesystem host asli).
- Test helper `internal/web/admin_users_test.go:newTestServer` membuat
  `Server` lengkap dengan DB sementara; setelah perubahan ke background scan,
  test harus memanggil `srv.refreshScan(context.Background())` secara manual
  sebelum request (scan tidak lagi otomatis jalan tanpa
  `StartBackgroundScan`).
- Build/test butuh toolchain Go lokal di `/tmp/go` (bukan dari package
  manager OS) — lihat `README.md` bagian "Build/test commands" untuk versi
  persis & alasan pin versi (`modernc.org/sqlite`, `x/crypto` pinned supaya
  cocok dengan Go 1.21).

## 9. Kalau mau menambah fitur discovery baru

Pola yang konsisten dipakai di `internal/discovery/`:
1. Ambil semua data **lewat panggilan read-only** (exec command atau HTTP
   GET), timeout wajar, gagal harus senyap (return string/nil kosong, jangan
   panic atau bikin seluruh `Scan()` gagal karena satu sumber data error).
2. Kalau panggilan bisa mahal & dilakukan per-item (per-slot/per-service),
   pertimbangkan concurrency (lihat pola `fetchRunningVersions` — goroutine +
   `sync.WaitGroup`) supaya total waktu scan tidak naik linear.
3. Jangan pernah menjadikan satu sumber data gagal sebagai alasan membuang
   data lain yang sudah berhasil didapat (lihat §4.2) — partial data lebih
   baik daripada halaman kosong.
4. Field baru di `Service`/`Slot` (`model.go`) harus dirender di
   `templates/dashboard.html` dengan fallback yang jelas (`{{if .Field}}`)
   supaya slot lama/service yang belum punya data itu tidak rusak
   tampilannya.
