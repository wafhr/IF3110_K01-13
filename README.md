# Database Management Platform

Web platform untuk mengelola akses bersama ke database PostgreSQL dan MySQL,
menelusuri schema dan data, serta menjalankan perubahan database melalui
migration yang tercatat. PostgreSQL tetap menyimpan data platform.

## Fitur

- Login username/password dengan session server-side, cookie aman, dan CSRF.
- Pengelolaan user, group, dan grant database oleh Platform Admin.
- Koneksi target PostgreSQL dan MySQL terenkripsi di platform registry.
- Viewer untuk schema, table, column, constraint, index, dan data.
- Definisi dan revisi SQL up/down, perencanaan rollback/forward linear, dan
  history commit PostgreSQL melalui API Milestone 2.
- Kontrak pengajuan berapproval dan eksekusi melalui antrean. Provider produksi
  approval, queue/worker, serta integrasi SPA sedang dikembangkan.
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

5. Buka `http://localhost:8080/login`, login dengan akun bootstrap, lalu
   daftarkan database target dan berikan grant melalui group.

Detail konfigurasi ada di [Panduan database management](docs/database-management.md),
[Access control](docs/access-control.md), dan [Migration](docs/migrations.md).
Panduan atribut, redaction, contoh output, pengujian, dan status integrasi worker
tersedia pada [Logging dan tracing](docs/logging-tracing.md).
API definisi/revisi dan history Milestone 2 dapat digunakan setelah server
berjalan. Pengajuan tindakan saat ini menghasilkan `503` sampai provider
approval dan antrean dipasang. Editor frontend sedang disesuaikan dengan alur
baru. Detail endpoint, payload, kontrak worker, dan status integrasi tersedia
pada panduan migration.

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
cmd/server/                 entry point dan wiring aplikasi
internal/access/            autentikasi, session, group, dan authorization
  schema.sql                tabel user, group, grant, dan session
internal/app/               routing komposisi modul
internal/config/            konfigurasi environment
internal/database/          registry target dan metadata domain
  schema.sql                tabel registry koneksi target
internal/database/http/     API registry dan viewer
internal/migration/         definisi/revisi, rollback/forward, history, dan kontrak eksekusi
internal/platformdb/        koneksi database platform
internal/targetdb/          pengelolaan koneksi target
internal/targetdb/drivers/ komposisi dan pemilihan driver target
internal/targetdb/mysql/    implementasi MySQL
internal/targetdb/postgres/ implementasi PostgreSQL
web/                        halaman, template, dan aset browser
tests/                      integration test database management dan migration
docs/                       panduan konfigurasi dan fitur
```

Schema platform berada di dekat paket yang memilikinya dan di-embed oleh
paket tersebut. Modul migration menyimpan `migration_definitions` dan
`migration_revisions` pada platform. History commit PostgreSQL menggunakan
`platform_migration_executions` pada target, sedangkan history Milestone 1
menggunakan `platform_migrations`. Metadata target disiapkan saat diperlukan.

Bonus DBMS tambahan yang didukung dan diuji adalah MySQL 8.4. Preview execution
plan memakai format native tiap DBMS; MySQL ANALYZE hanya berlaku untuk
statement yang didukung MySQL. Mode ANALYZE menjalankan SQL dan me-rollback
perubahan transaksional; sequence/auto-increment dan efek eksternal dapat tetap
berubah. Lihat [panduan schema diff](docs/migrations.md#schema-diff-and-sql-generation)
dan [preview plan](docs/migrations.md#query-plan-preview-explain).
