# Database Management Platform

Web platform untuk mengelola akses bersama ke database PostgreSQL dan MySQL,
menelusuri schema dan data, serta menjalankan perubahan database melalui
migration yang tercatat. PostgreSQL tetap menyimpan data platform.

## Fitur

- Login username/password dengan session server-side, cookie aman, dan CSRF.
- Pengelolaan user, group, dan grant database oleh Platform Admin.
- Koneksi target PostgreSQL dan MySQL terenkripsi di platform registry.
- Viewer untuk schema, table, column, constraint, index, dan data.
- Frontend Single-Page Application yang merangkum login, administrasi,
  database viewer, editor migration, review/approval, history, dan job.
- Migration dibuat dan direvisi sebagai SQL up/down, di-review, di-approve oleh
  leader setiap group yang memiliki akses (termasuk group View Only), lalu
  dieksekusi worker dalam satu transaksi: commit saat berhasil, rollback saat
  gagal.
- History linear per database mencatat versi, SQL, pelaksana, waktu, dan
  status, termasuk attempt yang gagal. Berlaku untuk PostgreSQL dan MySQL.
- Antrean job persisten, pemantauan progres, resume, dan kompensasi batch
  (PostgreSQL).
- Schema diff dari definisi JSON dan kandidat SQL untuk tabel/kolom yang belum
  ada; hasilnya dapat ditinjau dan diedit di editor sebelum dijalankan.
- Structured log JSON, audit aktivitas, tracing HTTP/SQL, serta propagasi trace
  pada payload pengajuan migration.
- Preview execution plan melalui `EXPLAIN` dan `EXPLAIN ANALYZE` di editor
  migration PostgreSQL dan MySQL 8.4.

Platform Admin mengelola platform; peran ini tidak memberi akses otomatis ke
database target. Hak `view_only` dan `view_edit` diberikan kepada group.

## Prasyarat

- Go 1.25 atau lebih baru (lihat versi modul di `go.mod`).
- PostgreSQL untuk database platform.
- Database PostgreSQL atau MySQL 8.4 target yang ingin dikelola.

## Menjalankan aplikasi

1. Buat database kosong untuk menyimpan data platform.
2. Salin `.env.example` menjadi `.env` pada root repository jika belum ada:

   ```powershell
   if (-not (Test-Path .env)) {
       Copy-Item .env.example .env
   }
   ```

   Aplikasi memuat `.env` otomatis saat startup. Environment yang sudah diatur
   dari luar aplikasi tetap diprioritaskan. `.env` diabaikan oleh Git;
   `.env.example` tetap menjadi template tanpa kredensial pribadi.
3. Isi `PLATFORM_DATABASE_URL`, `DATABASE_ENCRYPTION_KEY`,
   `BOOTSTRAP_ADMIN_USERNAME`, dan `BOOTSTRAP_ADMIN_PASSWORD` di `.env`.
   Password bootstrap minimum 12 karakter. Buat key dengan:

   ```powershell
   [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
   ```

   Simpan key yang sama setelah deployment pertama; menggantinya membuat
   password koneksi target yang sudah tersimpan tidak dapat didekripsi.
4. Jalankan server dari root repository:

   ```sh
   go run ./cmd/server
   ```

   Jalankan worker migration pada terminal lain dengan `.env` yang sama:

   ```sh
   go run ./cmd/worker
   ```

5. Buka `http://localhost:8080/login`, login dengan akun bootstrap, lalu
   daftarkan database target dan berikan grant melalui group.

Detail konfigurasi ada di [Panduan database management](docs/database-management.md),
[Access control](docs/access-control.md), dan [Migration](docs/migrations.md).
Panduan atribut, redaction, contoh output, pengujian, dan status integrasi worker
tersedia pada [Logging dan tracing](docs/logging-tracing.md).
Migration dijalankan dari halaman **Migrations** pada workspace database:
simpan migration, pilih **Review apply**, lalu setiap leader group yang memiliki
akses ke database tersebut menekan **Approve** (termasuk leader group View Only).
Setelah semua approval lengkap, user View + Edit menekan **Execute**. Worker
(`go run ./cmd/worker`) harus berjalan; jalankan server lebih dulu pada database
platform yang baru karena server membuat schema access control. Detail kontrak
backend tersedia pada [panduan migration](docs/migrations.md) dan
[panduan eksekusi](docs/migration-execution.md). Panduan halaman SPA, navigasi,
dan integrasi API tersedia di [docs/spa.md](docs/spa.md).

## Bonus Observability

Jalankan dari root repository dengan Docker Desktop aktif.

```sh
docker compose -f compose.observability.yaml up --build -d --wait
```

Compose menjalankan server, worker, PostgreSQL demo, OpenTelemetry Collector,
Prometheus, Loki, Tempo, dan Grafana. Database dan akun demo dibuat saat pertama
kali berjalan. Datasource dan dashboard Grafana dimuat otomatis, sehingga
tidak perlu menyiapkan `.env` atau mengimpor dashboard secara manual.

| Halaman | Username | Password demo |
| --- | --- | --- |
| [Aplikasi](http://localhost:8080/login) | `demo_admin` | `if3110-admin-demo` |
| [Dashboard IF3110 Observability](http://localhost:3000/d/if3110-observability) | `admin` | `if3110-grafana-demo` |

Untuk mencoba migration batch dan mengikuti trace-nya dari log atau metrik,
ikuti [skenario demo Observability](docs/logging-tracing.md#demo-migration-and-signal-correlation).
Jika port terpakai, atur `OBS_APP_PORT` atau `OBS_GRAFANA_PORT` sebelum menjalankan Compose.

Hentikan stack dengan `docker compose -f compose.observability.yaml down`.
Volume demo tetap tersedia untuk sesi berikutnya.

## Bonus Stress Testing dengan k6

Dari root repository, jalankan kedua skenario secara berurutan dengan Docker
aktif. Tiap skenario memakai server, worker, dan PostgreSQL sementara pada
jaringan Docker tersendiri. Perintah `down` menghapus data uji sementara agar
skenario berikutnya mulai dari kondisi awal yang sama.

```sh
docker compose -f compose.k6.yaml run --build --rm -e STRESS_TARGETS=same k6
docker compose -f compose.k6.yaml down
docker compose -f compose.k6.yaml run --build --rm -e STRESS_TARGETS=different k6
docker compose -f compose.k6.yaml down
```

[Skrip k6](k6/stress.js) mendaftarkan dua database target, mengatur grant
dan leader grup, dan membuat definisi migration melalui API. Saat beban
berjalan, pengguna virtual membaca data tabel pada kedua target dan
mengirim job secara bersamaan. Setiap job ditinjau dan disetujui sebelum
pengajuan. Mode `same` mengarahkan seluruh job ke satu target, sedangkan
`different` membagi job bergantian ke dua target. Setiap migration
memperbarui satu baris dan menambah waktu kerja SQL sintetis 350 ms.

| Fase (12 detik per fase) | Target baca tabel per detik | Target pengajuan job per detik |
| --- | ---: | ---: |
| Normal | 10 | 1 |
| Menengah | 30 | 4 |
| Puncak | 60 | 8 |
| Pemulihan | 10 | 1 |

Ambang tiap fase adalah **p95 respons API < 500 ms** dan **error rate < 1%**.
Keduanya dihitung dari request pembacaan tabel dan pengajuan job. Request
review, approval, dan pemantauan status tetap menghasilkan beban selama
pengujian. k6 mencetak ringkasan JSON dan memberikan exit code nonzero saat
ada ambang yang terlewati.

K6 memantau job yang diterima hingga selesai atau sampai batas pemantauan
15 detik. Waktu antrean dihitung dari waktu job dibuat sampai mulai dikerjakan.
Durasi migration dihitung dari waktu mulai hingga selesai untuk job yang
`succeeded`. Jumlah diterima, sukses, gagal, dan belum selesai dikelompokkan
berdasarkan fase pengajuan. Throughput job sukses dihitung berdasarkan waktu
job selesai, termasuk penyelesaian sesudah fase pemulihan yang dilaporkan
sebagai `drain`.

### Hasil pengujian lokal

Pengujian dijalankan berurutan di Docker Desktop (24 vCPU, 7.61 GiB RAM),
dengan PostgreSQL 18 di penyimpanan sementara dan satu worker dengan
concurrency 4. Kedua run memulai dari database kosong.

| Skenario | Fase | p50 API (ms) | p95 API (ms) | Request/s | Error API |
| --- | --- | ---: | ---: | ---: | ---: |
| Satu target | Normal | 17.3 | 43.1 | 11.17 | 0.00% |
| Satu target | Menengah | 17.0 | 39.5 | 34.17 | 3.17% |
| Satu target | Puncak | 17.5 | 40.9 | 68.17 | 0.98% |
| Satu target | Pemulihan | 16.8 | 38.5 | 11.08 | 0.00% |
| Dua target | Normal | 19.0 | 48.3 | 11.17 | 0.00% |
| Dua target | Menengah | 17.6 | 40.3 | 34.08 | 5.13% |
| Dua target | Puncak | 18.1 | 41.5 | 68.08 | 3.18% |
| Dua target | Pemulihan | 17.5 | 38.8 | 11.00 | 0.00% |

| Skenario | Fase | Diterima / sukses / gagal / belum selesai | p95 antrean (ms) | p95 durasi migration sukses (ms) | Job sukses/s |
| --- | --- | ---: | ---: | ---: | ---: |
| Satu target | Normal | 13 / 13 / 0 / 0 | 82.6 | 429.8 | 1.00 |
| Satu target | Menengah | 36 / 17 / 19 / 0 | 236.0 | 424.0 | 1.42 |
| Satu target | Puncak | 89 / 20 / 69 / 0 | 386.8 | 430.0 | 1.67 |
| Satu target | Pemulihan | 13 / 12 / 1 / 0 | 192.4 | 422.4 | 1.00 |
| Dua target | Normal | 13 / 13 / 0 / 0 | 34.0 | 436.8 | 1.00 |
| Dua target | Menengah | 28 / 26 / 2 / 0 | 50.3 | 428.8 | 2.17 |
| Dua target | Puncak | 70 / 34 / 36 / 0 | 240.1 | 444.4 | 2.75 |
| Dua target | Pemulihan | 12 / 11 / 1 / 0 | 140.9 | 427.0 | 1.08 |

- **Normal.** Semua job pada kedua skenario sukses. Beban ini dipakai
  sebagai pembanding untuk fase berikutnya.
- **Menengah.** Ambang error API terlewati pada kedua skenario. Dua target
  menyelesaikan 2.17 job per detik, dengan p95 antrean 50.3 ms. Pada satu
  target, hasilnya 1.42 job per detik dan 236.0 ms.
- **Puncak.** Dua target mencapai 2.75 job sukses per detik dan p95 antrean
  240.1 ms, dibanding 1.67 job per detik dan 386.8 ms pada satu target.
  Error API dua target masih melewati ambang. Seluruh p95 respons API
  di kedua run tetap di bawah ambang 500 ms.
- **Pemulihan.** Error API kembali ke 0%, tetapi p95 antrean masih lebih
  tinggi daripada fase normal. Satu job pada skenario satu target selesai
  setelah periode 12 detik pemulihan berakhir.

Pada kedua run, seluruh error API yang terukur terjadi saat pengajuan job.
Database platform mencatat `migration conflict` untuk 89 job gagal pada
satu target dan 39 pada dua target. Approval mengikat rencana ke riwayat
terakhir target. Saat job sebelumnya commit, rencana job lain pada target yang
sama dapat menjadi kedaluwarsa saat worker memvalidasi ulang. Kedua run tidak
menjatuhkan iterasi k6, dan semua job yang diterima mencapai status akhir.

## Bonus Security: SAST dengan CodeQL

[Workflow CodeQL](.github/workflows/codeql.yml) menganalisis repository setiap
push dan pull request ke `main` atau `develop`, setiap Senin, dan saat
dijalankan manual dari tab **Actions**. Hasilnya tampil di **Security → Code
scanning**. Ketiga bahasa memakai query suite `security-extended`.

| Bahasa | Cakupan |
| --- | --- |
| Go | 69 file non-test di `cmd/`, `internal/`, dan `web/` (dibangun dengan `go build ./...`) |
| JavaScript | 10 modul SPA di `web/static/js/`, `web/index.html`, dan `k6/stress.js` |
| GitHub Actions | Workflow di `.github/workflows/` |

Scan CodeQL 2.27.6 dengan konfigurasi yang sama menghasilkan dua temuan:

| Rule | Severity | Lokasi | Keputusan |
| --- | --- | --- | --- |
| `go/sql-injection` | High | `internal/migration/explain.go:69` | Risiko diterima. Preview `EXPLAIN` memang menjalankan SQL milik editor, dengan syarat View + Edit dan CSRF, satu statement, transaksi read-only atau selalu di-rollback, serta timeout 5 detik. Di GitHub, alert ini di-dismiss dengan alasan *Won't fix* |
| `js/client-side-request-forgery` | Medium | `web/static/js/pages/groups.js:107` | Diperbaiki di `451e33d`. Path request memakai ID group dari API yang di-encode, bukan nilai mentah `?group=`. Scan ulang tidak lagi melaporkan temuan JavaScript |

Analisis GitHub Actions tidak menghasilkan temuan. Hasil yang bersih atau
sedikit tidak berarti aplikasi pasti aman, karena CodeQL hanya mendeteksi pola
yang dicakup query-nya. Rincian alur data, dampak, alasan keputusan, teks
dismissal, dan cara menjalankan scan secara lokal ada di
[panduan CodeQL](docs/security-codeql.md).

## Test

Unit test (`*_test.go`) berada satu folder dengan package yang diuji.
Folder `tests/` hanya berisi integration test.

Jalankan unit dan test tanpa database:

```sh
go test ./...
```

Integration test menggunakan PostgreSQL dan MySQL disposable. Jalankan service
test dan test (perintah environment berikut untuk PowerShell):

```powershell
docker compose -f compose.test.yaml up -d --wait
$env:TEST_DATABASE_URL = 'postgres://if3110_test_admin:if3110-integration-password@localhost:55432/postgres?sslmode=disable'
$env:TEST_MYSQL_DSN = 'if3110_test_admin:if3110-mysql-test-password@tcp(127.0.0.1:53306)/if3110_mysql_test?tls=false'
go test -tags=integration ./tests
docker compose -f compose.test.yaml down -v
```

Integration test membuat dan menghapus database serta role dengan prefix
`if3110_it_` pada server tersebut. Gunakan hanya service test disposable, bukan
server atau database yang menyimpan data penting. Lihat [panduan test integrasi](docs/database-management.md#integration-test).

Untuk menjalankan integration test rollback/forward dan history PostgreSQL
secara khusus setelah `TEST_DATABASE_URL` tersedia, gunakan perintah berikut.

```powershell
go test -tags=integration -count=1 ./tests -run '^TestMigration.*Integration$'
```

## Struktur repo

```text
.github/workflows/          workflow CodeQL code scanning
cmd/server/                 entry point dan wiring aplikasi
cmd/worker/                 entry point worker eksekusi migration
internal/access/            autentikasi, session, group, dan authorization
  schema.sql                tabel user, group, grant, dan session
internal/app/               routing komposisi modul
internal/config/            konfigurasi environment
internal/database/          registry target dan metadata domain
  schema.sql                tabel registry koneksi target
internal/database/http/     API registry dan viewer
internal/job/               antrean job persisten dan API status job
internal/migration/         definisi/revisi, rollback/forward, history, dan kontrak eksekusi
internal/platformdb/        koneksi database platform
internal/targetdb/          pengelolaan koneksi target
internal/targetdb/drivers/ komposisi dan pemilihan driver target
internal/targetdb/mysql/    implementasi MySQL
internal/targetdb/postgres/ implementasi PostgreSQL
web/                        shell SPA dan aset browser
tests/                      integration test database management dan migration
docs/                       panduan konfigurasi dan fitur
```

Schema platform berada di dekat paket yang memilikinya dan di-embed oleh
paket tersebut. Modul migration menyimpan `migration_definitions` dan
`migration_revisions` pada platform. History commit PostgreSQL menggunakan
`platform_migration_executions` pada target, sedangkan history Milestone 1
menggunakan `platform_migrations`. Metadata target disiapkan saat diperlukan.

Bonus DBMS tambahan yang didukung dan diuji adalah MySQL 8.4, mencakup access
control, viewer, dan eksekusi migration apply/rollback/forward. MySQL melakukan
commit implisit untuk DDL, sehingga kegagalan setelah DDL tidak dapat
membatalkan DDL tersebut; perubahan data tetap di-rollback. Batch migration
hanya tersedia untuk PostgreSQL. Preview execution
plan memakai format native tiap DBMS; MySQL ANALYZE hanya berlaku untuk
statement yang didukung MySQL. Mode ANALYZE menjalankan SQL dan me-rollback
perubahan transaksional; sequence/auto-increment dan efek eksternal dapat tetap
berubah. Lihat [panduan schema diff](docs/migrations.md#schema-diff-and-sql-generation)
dan [preview plan](docs/migrations.md#query-plan-preview-explain).
