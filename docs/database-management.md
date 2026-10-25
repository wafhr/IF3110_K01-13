# Database management

Platform menggunakan satu PostgreSQL sebagai database platform untuk user,
group, session, grant, dan registry koneksi. Database yang didaftarkan admin
adalah target PostgreSQL yang terpisah.

## Kepemilikan schema

Schema platform dipisahkan menurut modul pemiliknya:

- `internal/access/schema.sql` membuat tabel user, group, membership, grant,
  dan session. Paket `internal/access` meng-embed dan menjalankannya.
- `internal/database/schema.sql` membuat tabel registry `managed_databases`.
  Paket `internal/database` meng-embed dan menjalankannya.
- `platform_migrations` bukan tabel platform. Tabel itu dibuat oleh
  `internal/migration` pada setiap database target ketika riwayat migration
  pertama kali dibaca atau ditulis.

Dua file `schema.sql` tersebut bukan duplikasi: keduanya memiliki tabel,
pemilik, dan siklus inisialisasi berbeda. Menaruhnya di folder `db/` terpisah
belum memberi manfaat pada pola saat ini karena setiap modul meng-embed schema
lokalnya. Folder migrasi terpusat baru masuk akal jika schema kelak dikelola
oleh satu migration runner dengan versi dan urutan aplikasi yang eksplisit.

Grant menyimpan UUID registry dan tidak memakai foreign key lintas schema.
Access control memvalidasi bahwa database terdaftar saat grant dibuat;
penghapusan database menghapus grant terkait dan mencabut session yang
terdampak dalam satu transaksi. Integrasi ini perlu dipertahankan bila modul
registry atau access control diubah.

## Konfigurasi koneksi

Atur `PLATFORM_DATABASE_URL` untuk database platform dan
`DATABASE_ENCRYPTION_KEY` sebagai base64 dari 32 byte acak. Contoh pembuatan
key di PowerShell:

```powershell
[Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
```

Registry mengenkripsi password koneksi target menggunakan AES-256-GCM. Simpan
key secara persisten di secret manager; kehilangan atau penggantian key
mencegah aplikasi membuka kembali password koneksi yang sudah disimpan.
Password target tidak dikembalikan oleh endpoint registry.

Saat startup pertama pada database platform kosong, aplikasi membuat Platform
Admin dari `BOOTSTRAP_ADMIN_USERNAME` dan `BOOTSTRAP_ADMIN_PASSWORD`. Password
bootstrap harus memiliki minimal 12 karakter. Nilai bootstrap tidak mengubah
akun setelah pembuatan pertama.

File `.env.example` berisi daftar variabel, tetapi aplikasi tidak memuat file
`.env` otomatis. Set variabel di shell, environment deployment, atau secret
manager. `APP_ENV=production` mengaktifkan atribut Secure pada cookie; untuk
TLS yang berakhir di reverse proxy, set juga `COOKIE_SECURE=true`.

## Registry target

Platform Admin mendaftarkan koneksi pada `/manage/databases`. API yang tersedia:

| Method | Path | Fungsi |
|---|---|---|
| `GET` | `/admin/databases` | Daftar koneksi terdaftar |
| `POST` | `/admin/databases` | Daftarkan koneksi baru |
| `GET` | `/admin/databases/{databaseID}` | Baca metadata koneksi |
| `PUT` | `/admin/databases/{databaseID}` | Ganti metadata dan kredensial koneksi |
| `DELETE` | `/admin/databases/{databaseID}` | Hapus registrasi dan grant terkait |
| `POST` | `/admin/databases/test-connection` | Uji konfigurasi sebelum menyimpan |
| `POST` | `/admin/databases/{databaseID}/test-connection` | Uji koneksi tersimpan |

Endpoint menerima field `name`, `host`, `port`, `database_name`, `username`,
`password`, dan `ssl_mode` dalam body JSON. Port default `5432` dan SSL mode
default `require`. Semua operasi pengelolaan memerlukan Platform Admin dan
token CSRF. Perubahan atau penghapusan registrasi menginvalidasi pool koneksi
yang tersimpan.

Setelah registrasi, admin memberikan grant database UUID kepada group. User
hanya melihat database yang diberikan melalui group; aturan permission efektif
dijelaskan dalam [Access control](access-control.md).

## Viewer

Endpoint viewer memerlukan session valid dan permission `view_only` atau lebih:

| Method | Path | Fungsi |
|---|---|---|
| `GET` | `/databases` | Daftar database yang dapat diakses user |
| `GET` | `/databases/{databaseID}/schemas` | Daftar schema yang terlihat |
| `GET` | `/databases/{databaseID}/schemas/{schema}/tables` | Daftar table pada schema |
| `GET` | `/databases/{databaseID}/schemas/{schema}/tables/{table}` | Detail column, constraint, dan index |
| `GET` | `/databases/{databaseID}/schemas/{schema}/tables/{table}/data` | Halaman data table |

Endpoint data menerima `limit` (default 50, maksimum 200) dan `offset`
(default 0). Nilai dikembalikan sebagai teks atau `null` untuk mempertahankan
presisi tipe PostgreSQL. Akun target perlu hak `USAGE` pada schema dan `SELECT`
pada data yang akan dibaca.

Kegagalan koneksi, autentikasi target, atau izin target dikembalikan sebagai
respons aman tanpa mengungkap credential koneksi.

## Integration test

Integration test membutuhkan Docker dan hanya boleh dijalankan terhadap server
PostgreSQL disposable. Konfigurasi server sudah tersedia di
`compose.test.yaml`:

```powershell
docker compose -f compose.test.yaml up -d --wait
$env:TEST_DATABASE_URL = 'postgres://if3110_test_admin:if3110-integration-password@localhost:55432/postgres?sslmode=disable'
go test -tags=integration ./tests/database_management/integration
docker compose -f compose.test.yaml down -v
```

Test membuat database platform dan target sementara serta role target dengan
prefix `if3110_it_`, lalu menghapusnya saat cleanup. Akun yang dipakai harus
memiliki izin membuat/menghapus database dan role. Jangan arahkan
`TEST_DATABASE_URL` ke server yang menyimpan data penting.
