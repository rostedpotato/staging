# Staging Platform API

Referensi untuk sistem atau skrip yang mengambil data dari platform. Dokumen
interaktif yang selalu sesuai dengan server ada di **`/api/docs`** (Swagger UI,
bisa dicoba langsung) dan spec mentahnya di **`/api/openapi.yaml`**.

Saat ini API bersifat **baca-saja (read-only)**: tidak ada endpoint untuk
membuat, mengubah, atau menghapus data.

## Daftar endpoint

| Method | Path | Auth | Fungsi |
|---|---|---|---|
| `GET` | `/api/v1/bookings` | API key, scope `bookings:read` | Booking env pada rentang waktu |
| `GET` | `/api/docs` | tidak perlu | Dokumentasi interaktif (Swagger UI) |
| `GET` | `/api/openapi.yaml` | tidak perlu | Spec OpenAPI 3.0 |
| `GET` | `/healthz` | tidak perlu | Cek server hidup (`ok`) |

Endpoint lain (halaman web, `/api/slots`, dan sebagainya) memakai login
browser, **bukan** API key, dan tidak bisa dipanggil dengan key.

## Persiapan

```sh
BASE=http://10.20.1.12:8088            # alamat platform
KEY=<api key yang diberikan admin>
H="Authorization: Bearer $KEY"
```

Cek koneksi dan key:

```sh
curl -s $BASE/healthz                                   # -> ok
curl -s -H "$H" "$BASE/api/v1/bookings?from=2026-10-01&to=2026-10-01&limit=1"
```

Key dibuat oleh admin dan hanya ditampilkan sekali. Cara meminta atau membuat
key ada di `README.md` bagian *Issuing an API key*. Kirim key hanya melalui
jaringan tepercaya atau HTTPS.

## Autentikasi

Setiap panggilan ke `/api/v1/*` wajib menyertakan header:

```
Authorization: Bearer <api key>
```

- Cookie login browser **tidak** diterima di sini.
- Setiap key punya daftar *scope*. Key tanpa scope yang dibutuhkan ditolak 403.
- Pemakaian key dicatat di log server (`api: key="<nama>" GET ...`).

## `GET /api/v1/bookings`

Mengembalikan booking yang jendelanya **beririsan** dengan rentang
`[from, to)`, yaitu `start < to` dan `end > from`. Booking yang mulai sebelum
`from` tetapi masih berjalan tetap ikut.

### Parameter query

| Parameter | Wajib | Keterangan |
|---|---|---|
| `from` | ya | Awal rentang. `YYYY-MM-DD` (pukul 00:00 WIB) atau RFC3339 |
| `to` | ya | Akhir rentang. Jika berupa tanggal, **seluruh hari itu ikut**. Harus setelah `from`, maksimal 366 hari dari `from` |
| `slot` | tidak | Nama env persis, mis. `development-04` atau `0vt` |
| `status` | tidak | `active`, `expired`, `released`, atau `overridden` |
| `limit` | tidak | Ukuran halaman, 1–500, default 100 |
| `offset` | tidak | Jumlah data yang dilewati, default 0 |

Arti `status`:

| Nilai | Arti |
|---|---|
| `active` | sedang berjalan atau akan datang |
| `expired` | berjalan sampai akhir jadwal |
| `released` | diakhiri lebih awal oleh pemiliknya |
| `overridden` | diakhiri lebih awal oleh admin |

Format waktu RFC3339 harus memakai offset, mis. `2026-10-15T08:00:00+07:00`.
Jika menulis URL langsung, tulis `+` sebagai `%2B`; dengan
`curl -G --data-urlencode` hal itu otomatis.

### Contoh panggilan

```sh
# semua booking Oktober
curl -s -H "$H" "$BASE/api/v1/bookings?from=2026-10-01&to=2026-10-31"

# satu env, hanya yang berjalan penuh
curl -s -H "$H" "$BASE/api/v1/bookings?from=2026-10-01&to=2026-10-31&slot=development-04&status=expired"

# jam tertentu
curl -s -G -H "$H" "$BASE/api/v1/bookings" \
  --data-urlencode "from=2026-10-15T08:00:00+07:00" \
  --data-urlencode "to=2026-10-15T18:00:00+07:00"

# halaman kedua, 20 data per halaman
curl -s -H "$H" "$BASE/api/v1/bookings?from=2026-10-01&to=2026-10-31&limit=20&offset=20"
```

### Contoh respons (200)

```json
{
  "data": [
    {
      "id": 42,
      "slot": "development-04",
      "username": "brian",
      "name": "Brian",
      "team": "QA",
      "purpose": "dev multiple zone gp",
      "start": "2026-10-15T00:00:00+07:00",
      "end": "2026-10-22T23:59:59+07:00",
      "effectiveEnd": "2026-10-20T11:30:00+07:00",
      "status": "released",
      "createdAt": "2026-10-12T09:41:07+07:00",
      "releasedAt": "2026-10-20T11:30:00+07:00"
    }
  ],
  "meta": {
    "from": "2026-10-01T00:00:00+07:00",
    "to": "2026-11-01T00:00:00+07:00",
    "limit": 100,
    "offset": 0,
    "total": 1
  }
}
```

| Field | Arti |
|---|---|
| `id` | ID booking |
| `slot` | Nama env |
| `username` | Username akun yang membuat booking (tetap tersedia di API) |
| `name` / `team` | Nama dan tim yang diisi saat booking |
| `purpose` | Tujuan booking |
| `start` / `end` | Jadwal rencana. Booking per hari penuh berakhir `23:59:59` hari terakhir |
| `effectiveEnd` | Kapan env benar-benar berhenti dipegang. Sama dengan `end`, atau waktu release jika dilepas lebih awal |
| `status` | Lihat tabel status di atas |
| `createdAt` | Kapan booking dibuat |
| `releasedAt` | Waktu release; `null` selama masih aktif |
| `meta.from` / `meta.to` | Rentang yang dipakai server (`to` eksklusif) |
| `meta.total` | Jumlah seluruh hasil untuk filter ini, di semua halaman |

Hasil diurutkan berdasarkan `start`, lalu `id`. Hasil kosong adalah
`"data": []` (bukan `null`) dengan status 200.

Klien sebaiknya **mengabaikan field yang tidak dikenal**, karena field baru
bisa ditambahkan tanpa mengganti versi.

## Format error

Semua error berbentuk sama dan tidak pernah berupa redirect:

```json
{ "error": { "code": "invalid_request", "message": "to must be after from" } }
```

| HTTP | `code` | Penyebab |
|---|---|---|
| 400 | `invalid_request` | Parameter hilang atau tidak valid (lihat `message`) |
| 401 | `unauthorized` | Header tidak ada, salah format, atau key tidak dikenal |
| 403 | `forbidden` | Key valid tetapi tanpa scope yang dibutuhkan |
| 404 | `not_found` | Path `/api/v1/...` tidak ada |
| 405 | `method_not_allowed` | Bukan `GET` |
| 500 | `internal` | Kesalahan server; coba lagi, lalu hubungi admin |

Penyebab 400 yang paling umum: `from`/`to` tidak ada, format tanggal salah,
`to` tidak setelah `from`, jarak lebih dari 366 hari, `status` di luar empat
nilai, `limit` di luar 1–500, atau `offset` negatif.

Jika `401` padahal key benar: pastikan tidak ada spasi atau baris baru di
`$KEY`, dan server sudah di-restart setelah `config.json` diubah.

## Skenario penggunaan

Semua skenario memakai `BASE`, `KEY`, dan `H` dari bagian *Persiapan* dan
membutuhkan `curl` serta `jq`.

### Skenario 1 – Laporan bulanan pemakaian env

Kebutuhan: tim ingin tahu siapa memakai env apa selama Oktober 2026.

```sh
curl -s -H "$H" "$BASE/api/v1/bookings?from=2026-10-01&to=2026-10-31&limit=500" \
 | jq -r '.data[] | [.slot, .name, .team, .start[:10], .effectiveEnd[:10], .status, .purpose] | @tsv'
```

Laporan ini memakai nama dan tim agar hasilnya mudah dibaca. Field `username`
tetap tersedia jika laporan juga perlu mencantumkan identitas akun.

Jumlah booking per env:

```sh
curl -s -H "$H" "$BASE/api/v1/bookings?from=2026-10-01&to=2026-10-31&limit=500" \
 | jq -r '.data | group_by(.slot)[] | "\(.[0].slot)\t\(length)"'
```

Jika `meta.total` lebih dari 500, ambil semua halaman dengan Skenario 3.

### Skenario 2 – Siapa yang memegang env ini sekarang?

Kebutuhan: sebelum meminta env kepada seseorang, cek siapa yang sedang
memakainya.

```sh
SLOT=development-04
curl -s -G -H "$H" "$BASE/api/v1/bookings" \
  --data-urlencode "from=$(date -Iseconds)" \
  --data-urlencode "to=$(date -d '+1 minute' -Iseconds)" \
  --data-urlencode "slot=$SLOT" \
  --data-urlencode "status=active" \
 | jq -r 'if .meta.total == 0 then "'"$SLOT"' sedang kosong"
          else .data[] | "\(.slot) dipakai \(.name)\(if .team == "" then "" else " (\(.team))" end) sampai \(.end[:16]) (\(.purpose))" end'
```

Rentang satu menit di sekitar "sekarang" membuat hanya booking yang sedang
berjalan yang cocok. Untuk melihat juga yang akan datang hari ini, ganti
rentangnya menjadi `from=$(date +%F)&to=$(date +%F)`.

### Skenario 3 – Ekspor semua booking ke CSV (banyak halaman)

Kebutuhan: ekspor seluruh booking Q4 2026 ke file untuk dibuka di spreadsheet.
Jumlahnya bisa melebihi 500, jadi ambil halaman demi halaman sampai `total`
terpenuhi.

```sh
FROM=2026-10-01; TO=2026-12-31; LIMIT=500; OFFSET=0
echo "id,slot,username,name,team,start,effective_end,status,purpose" > bookings.csv

while :; do
  RESP=$(curl -sf -H "$H" \
    "$BASE/api/v1/bookings?from=$FROM&to=$TO&limit=$LIMIT&offset=$OFFSET") || { echo "request gagal" >&2; exit 1; }

  echo "$RESP" | jq -r '.data[] | [.id,.slot,.username,.name,.team,.start,.effectiveEnd,.status,.purpose] | @csv' >> bookings.csv

  TOTAL=$(echo "$RESP" | jq '.meta.total')
  OFFSET=$((OFFSET + LIMIT))
  [ "$OFFSET" -ge "$TOTAL" ] && break
done
wc -l bookings.csv
```

`curl -f` membuat skrip berhenti jika server membalas error (401, 400, dan
sebagainya). Rentang maksimal per panggilan 366 hari; untuk periode lebih
panjang, jalankan per tahun.

### Skenario 4 – Audit booking yang dilepas lebih awal

Kebutuhan: lihat booking yang dilepas atau ditimpa admin sebelum jadwal selesai,
beserta berapa hari lebih cepat.

```sh
for S in released overridden; do
  echo "== $S"
  curl -s -H "$H" "$BASE/api/v1/bookings?from=2026-10-01&to=2026-10-31&status=$S&limit=500" \
   | jq -r '.data[] | "\(.slot)\t\(.name)\(if .team == "" then "" else " (\(.team))" end)\tjadwal \(.end[:10])\tselesai \(.effectiveEnd[:10])"'
done
```

### Skenario 5 – Skrip Python untuk laporan harian

Kebutuhan: skrip terjadwal (cron) yang setiap pagi mendaftar env yang sedang
dipegang beserta pemiliknya, dan menandai booking yang **berakhir hari ini**
supaya env-nya bisa segera dipakai orang lain.

```python
import os, datetime as dt, requests

BASE = os.environ["BASE"]
KEY = os.environ["KEY"]
today = dt.date.today().isoformat()

resp = requests.get(
    f"{BASE}/api/v1/bookings",
    headers={"Authorization": f"Bearer {KEY}"},
    params={"from": today, "to": today, "status": "active", "limit": 500},
    timeout=10,
)
resp.raise_for_status()           # 4xx/5xx -> exception, skrip gagal terlihat
body = resp.json()

print(f"{today}: {body['meta']['total']} booking aktif")
for b in sorted(body["data"], key=lambda b: b["slot"]):
    ends = b["end"][:10]
    note = "  <-- berakhir hari ini" if ends == today else ""
    team = f" ({b['team']})" if b.get("team") else ""
    print(f"  {b['slot']}: {b['name']}{team} sampai {ends} ({b['purpose']}){note}")
```

Keluaran contoh:

```
2026-10-23: 2 booking aktif
  0pv: Dewi (QA) sampai 2026-10-23 (sedang jalan juga)  <-- berakhir hari ini
  development-04: Citra (Platform) sampai 2026-10-25 (sedang jalan)
```

Menjalankannya:

```sh
BASE=http://10.20.1.12:8088 KEY=... python3 laporan.py
```

Simpan `KEY` di environment atau secret manager milik cron/CI, jangan ditulis di
dalam skrip yang masuk repositori.

## Batasan saat ini

- Hanya baca; belum ada endpoint untuk membuat atau melepas booking.
- Belum ada endpoint daftar env, jadi daftar env yang "kosong" harus
  dibandingkan sendiri dengan daftar env yang Anda ketahui.
- Rentang maksimal 366 hari per panggilan dan 500 data per halaman.
- Belum ada pembatasan laju (rate limit); gunakan secukupnya.

Perubahan API akan ditambahkan di bawah `/api/v1` selama kompatibel (field atau
endpoint baru). Perubahan yang merusak kompatibilitas akan memakai `/api/v2`.
