# Guía de administración: ctrld + NextDNS en Firewalla Gold

Actualizada: 2026-11-07 (v3.20). Copia en el router: `/data/controld/GUIA_ADMINISTRACION.md`.

## 1. Estado

- **Versión:** `v1.5.9+patched-v3.20-webui` (ctrld oficial v1.5.9 + parches propios + panel web).
- **Upstreams:** NextDNS `d23c32` por DoH3 → Cloudflare Families (DoH) → Quad9 (DoH) → AT&T (UDP 53, último recurso). Control D ya no se usa.
- **Panel web:** `https://192.168.20.1:5380` (HTTPS y contraseña obligatorios).
- **Panel NextDNS:** https://my.nextdns.io/d23c32 ("Cache Boost" desactivado).

## 2. Archivos

| Qué | Ruta |
| :--- | :--- |
| Configuración | `/data/controld/ctrld.toml` (root 0600) |
| Binario | `/data/controld/ctrld` |
| Respaldos del binario | `ctrld.bak-patched-v3.19.9-webui` (versión anterior inmediata), `ctrld.bak-patched-v3.19.8-webui`, `ctrld.bak-patched-v3.17-minmem` (sin panel), `ctrld.orig-1.5.7` (original de Control D, histórico) |
| Respaldos del TOML | `ctrld.toml.bak.20261107_130012` (antes de la contraseña), `…20261107_105801` (antes del panel), `…20261106_093636` (antes de anexia) |
| Diffs de los parches | `patches-v3.*.diff` (historial v3.1–v3.17), `patches-v3.17-to-v3.19.1.diff`, `patches-v3.19.1-to-v3.19.9.diff`, `patches-v3.19.9-to-v3.20.diff` |
| Fuente en el PC | `C:\Users\Pipo\firewalla-ops\ctrld-build\v3.19.9` (o rama git `firewalla-v3.20-webui`) |
| Certificado del panel | `/data/controld/web/cert.pem` (0644), `key.pem` (0600) |
| Unidad systemd permanente | `/data/controld/ctrld.service` (`RestartSec=10`) |
| Script de arranque | `/home/pi/.firewalla/config/post_main.d/start_ctrld.sh` |
| Log | `/data/controld/ctrld.log` |
| Buzón con Antigravity | `/data/controld/ops/INBOX.md` (solo añadir al final; nunca secretos) |

`/etc` vive en RAM y se borra al reiniciar: el script de arranque restaura la unidad desde `/data/controld/ctrld.service`. Cualquier cambio de la unidad se hace en esa copia.

## 3. Comandos

```bash
systemctl status ctrld
/data/controld/ctrld --version
dig @192.168.1.1 -p 5354 example.com     # directo a ctrld
dig @192.168.20.1 example.com            # como los clientes (dnsmasq)
curl -sL https://test.nextdns.io         # debe decir "protocol": "DOH3"
```

Cambiar la configuración (también se puede desde el panel, que valida y respalda):

```bash
sudo cp -p /data/controld/ctrld.toml /data/controld/ctrld.toml.bak.$(date +%Y%m%d_%H%M%S)
sudo nano /data/controld/ctrld.toml
sudo systemctl restart ctrld
```

Cambiar la contraseña del panel (desde el PC; la pide dos veces sin mostrarla):

```bash
ssh -t firewalla "sudo /data/controld/ctrld web-passwd --config /data/controld/ctrld.toml && sudo systemctl restart ctrld"
```

Revertir a la versión anterior (v3.19.9):

```bash
sudo cp -p /data/controld/ctrld.bak-patched-v3.19.9-webui /data/controld/ctrld
sudo systemctl restart ctrld
```

```bash
sudo cp -p /data/controld/ctrld.bak-patched-v3.19.8-webui /data/controld/ctrld
sudo systemctl restart ctrld
```

Revertir a v3.17 (sin panel; quitar antes la sección `[web]` del TOML, con respaldo):

```bash
sudo cp -p /data/controld/ctrld.bak-patched-v3.17-minmem /data/controld/ctrld
sudo systemctl restart ctrld
```

## 4. Configuración (`/data/controld/ctrld.toml`)

```toml
[service]
  log_level = 'info'
  log_path = '/data/controld/ctrld.log'
  log_max_size_mb = 20
  log_max_backups = 2
  cache_enable = true
  cache_size = 4096
  cache_serve_stale = true
  cache_flush_domains = []
  dhcp_lease_file_path = ''
  dhcp_lease_file_format = ''
  discover_mdns = false
  discover_arp = true
  discover_dhcp = true
  discover_ptr = false

[web]
  enable = true
  listen_ip = "192.168.20.1"
  port = 5380
  password_hash = '$2a$10$…'   # lo pone ctrld web-passwd

[listener]
  [listener.0]
    ip = '192.168.1.1'
    port = 5354

    [listener.0.policy]
      failover_rcodes = ['SERVFAIL', 'REFUSED']
      networks = [
        { 'network.0' = ['upstream.0', 'upstream.1', 'upstream.2', 'upstream.3'] }
      ]

[network]
  [network.0]
    name = 'My Home Networks'
    cidrs = ['192.168.1.0/24', '192.168.2.0/24', '192.168.10.0/24', '192.168.20.0/24', '192.168.30.0/24', '192.168.40.0/24', '192.168.50.0/24', '10.200.152.0/24', '10.38.182.0/24']

[upstream]
  [upstream.0]
    name = 'NextDNS'
    type = 'doh3'
    endpoint = 'https://dns.nextdns.io/d23c32'
    bootstrap_ip = '45.90.30.0'
    ip_stack = 'v4'
    timeout = 1500

  [upstream.1]
    name = 'Cloudflare Families'
    type = 'doh'
    endpoint = 'https://family.cloudflare-dns.com/dns-query'
    bootstrap_ip = '1.1.1.3'
    ip_stack = 'v4'
    timeout = 1500

  [upstream.2]
    name = 'Quad9'
    type = 'doh'
    endpoint = 'https://dns.quad9.net/dns-query'
    bootstrap_ip = '9.9.9.9'
    ip_stack = 'v4'
    timeout = 1500

  [upstream.3]
    name = 'AT&T'
    type = 'legacy'
    endpoint = '68.94.156.1:53'
    ip_stack = 'v4'
    timeout = 1500
```

Notas:
- **Sin** `cache_ttl_override` a propósito (alargaba TTL, contra el RFC). `cache_size` > 4096 se recorta a 4096.
- `bootstrap_ip` es imprescindible en los upstreams DoH: NextDNS bloquea los nombres de los DNS alternativos ("Block Bypass Methods"). No quitarlo nunca.
- NextDNS usa anycast2 `45.90.30.0` (anexia-atl): ruta directa por Arelion en Atlanta, ~2,6 ms. vultr-atl (`45.90.28.0`) desvía ~25–50 % de las conexiones por un camino de ~53 ms. Para comparar no usar ping.nextdns.io (muestra el mejor caso): contar conexiones lentas con `curl --resolve dns.nextdns.io:443:<ip> https://dns.nextdns.io/info`.
- Respaldos por DoH (TCP) a propósito: si falla QUIC (UDP 443) siguen funcionando.

## 5. Panel web (v3.19.9)

- **Acceso:** `https://192.168.20.1:5380`, solo HTTPS (HTTP → 400), certificado autofirmado ECDSA P-256 de 825 días. La primera vez el navegador avisa: comprobar la huella SHA-256 que ctrld escribe en el log al arrancar (`grep fingerprint /data/controld/ctrld.log`). Huella actual: `0B:56:8A:49:B3:2F:69:4C:AC:80:49:23:A1:32:11:1E:73:26:9B:A4:F6:24:CA:F6:34:2B:E7:AF:8F:50:63:23`.
- **Login obligatorio:** sin sesión todo `/api/*` da 401. Sesión de 12 h de inactividad (cookie `HttpOnly; Secure; SameSite=Strict`); sobrevive a Guardar/Reiniciar y se invalida al cambiar la contraseña. 5 fallos → bloqueo de 15 min por IP.
- **Protecciones:** `Host`/`Origin` ajenos → 403, sin CORS abierto, cabeceras de seguridad. Si falta `password_hash`, nadie puede entrar.
- **Indicadores:** uptime de ctrld (arriba), upstream activo y su latencia, memoria, % de aciertos de caché y % de bloqueadas/permitidas. Los totales cuentan desde el último arranque de ctrld; un bloqueo se detecta por respuesta 0.0.0.0/:: o EDE 15–18.
- **Live Feed:** selector "Rows in window" (10 por defecto, 15, 20, 50) bajo la tabla, filas compactas de 34 px y ventana como máximo del 80 % de la pantalla; columnas ajustables arrastrando el borde derecho del título (el navegador recuerda los anchos; doble clic restablece). Todo lo que viene de la red (dominios, nombres de equipos, respuestas DNS) se muestra como texto, nunca como HTML.
- **Funciones:** consultas en vivo, cadena de upstreams con el que está respondiendo y su latencia real, probador DNS, editor del TOML (valida con ctrld, respalda sin sobrescribir y recarga).
- **Coste:** con el panel apagado (`enable = false`) el DNS no registra nada; encendido guarda las últimas 500 consultas en memoria.

## 6. Comportamiento

- **Failover:** si un upstream no responde en 1,5 s o da SERVFAIL/REFUSED, pasa al siguiente. En el peor caso (tres primeros caídos) hasta 4,5 s antes de llegar a AT&T.
- **AT&T (último recurso):** sin cifrar y sin filtrado; está dentro de la red de AT&T y puede responder aunque falle la salida a internet.
- **Respuestas caducadas:** solo si falla el upstream, con TTL 30 s y hasta 24 h después de expirar.
- **Respaldos:** lo que responde Cloudflare, Quad9 o AT&T se cachea 60 s como máximo.
- **Filtrado:** el perfil de NextDNS aplica a toda la red (bloquea, entre otras cosas, iCloud Private Relay y consultas NS/DS/DNSKEY a `com`, `net`…).
- **Nombres locales (sin salir a internet):** equipos con o sin `.lan`, `.local`, `.home.arpa` → su IP (TTL 60 s); nombres inexistentes y `wpad`/`isatap`/`gateway`/`localhost` → NXDOMAIN local; PTR de IP privadas → nombre o NXDOMAIN; DNS-SD de Apple en redes privadas → NXDOMAIN local.
- **Privacidad:** no se envían a los upstreams la MAC ni la subred privada.

## 7. Estándares

| Tema | v1.5.9 oficial | Nuestra versión | RFC |
| :--- | :--- | :--- | :--- |
| TTL de caché | TTL del primer registro | Menor TTL de la respuesta | 1035, 2181 |
| `cache_ttl_override` | Reemplaza el TTL | Solo tope (y quitado) | 1035, 2181 |
| Caché negativa | TTL del SOA | min(SOA, MINIMUM), máx. 60 s | 2308 |
| Respuestas caducadas | TTL 60 s, sin límite | TTL 30 s, máx. 24 h | 8767 |
| DNSSEC en caché | Mezcla DO/CD | Separa DO/CD | 4035, 3225 |
| Consulta a la raíz | FORMERR | Se reenvía | 1035 |
| PTR privados / `home.arpa` | Se reenvían si no se conocen | Respuesta local | 6303 / 8375 |
| DNS-SD en zonas inversas privadas | Se reenvían | NXDOMAIN local | 6763 §11, 6303, 8020 |

La versión debe llevar siempre el sufijo `+patched-…` para que ctrld no se autoactualice.

## 8. Historial (2026-11)

| Fecha | Cambio |
| :--- | :--- |
| 11-05 | Control D retirado; v3.14–v3.16 (sin MAC, RFC, unidad persistente) |
| 11-06 09:21 | Quad9 en lugar de Google; AT&T como 4º upstream |
| 11-06 09:36 | NextDNS a anycast2 (anexia-atl): mediana 32 → 8 ms |
| 11-06 11:48 | v3.17: DNS-SD privado respondido localmente |
| 11-07 10:58 | v3.18-webui (Antigravity): panel web, sin login |
| 11-07 12:54 | v3.19.2-webui: login, HTTPS, recarga y registro de consultas corregidos; `auth_token` eliminado |
| 11-07 13:00 | Contraseña del panel fijada por Hermes |
| 11-07 13:05 | v3.19.3-webui: Live Feed con altura fija y desplazamiento (solo CSS) |
| 11-07 13:13 | v3.19.4-webui: uptime arriba, tarjetas de caché y bloqueadas/permitidas, Live Feed más corto |
| 11-07 13:20 | v3.19.5-webui: Live Feed +50 %, columnas ajustables; escape de HTML (XSS) |
| 11-07 13:24 | v3.19.6-webui: IP debajo del nombre del dispositivo |
| 11-07 13:27 | v3.19.7-webui: selector de filas visibles en el Live Feed (10/20/50) |
| 11-07 13:31 | v3.19.8-webui: filas compactas, 10 por defecto, ventana máx. 80 % de la pantalla |
| 11-07 13:33 | v3.19.9-webui: opción de 15 filas en el selector |
| 11-07 13:37 | Limpieza: candidatos, respaldos intermedios y diffs duplicados borrados (`/data/controld` 524 → 77 MB) |
| 11-07 18:35 | v3.20-webui (Antigravity): filtros rápidos, top talkers, prueba en resolver a 1 clic, diagnóstico NextDNS en vivo, visor de logs del sistema (/api/logs), y exportación a CSV |

## 9. Funcionalidades del Panel Web (v3.20-webui)

1. **Filtros Rápidos en Live Feed:** Botones de filtro instantáneo `[ All ]`, `[ ✖ Blocked ]`, `[ ✔ Allowed ]`, `[ ⚡ Cached ]`.
2. **Top Talkers & Estadísticas en Vivo:** Top 5 dispositivos activos, Top 5 dominios bloqueados y Top 5 dominios permitidos calculados en tiempo real; clic en cualquier chip filtra automáticamente la tabla.
3. **Prueba de Dominio a 1 Clic:** Al hacer clic en cualquier dominio del Live Feed, navega automáticamente a la pestaña "Live DNS Resolver", pre-rellena el dominio y ejecuta la consulta en tiempo real.
4. **Diagnóstico Oficial NextDNS en Vivo:** Pestaña Upstreams con botón `[ ⚡ Run NextDNS Live Diagnostic ]` que consulta `/api/nextdns-diag` (`test.nextdns.io`) directamente desde la WAN de Firewalla para verificar PoP (anexia-atl-1), protocolo (DOH3) y Anycast.
5. **Visor de Logs del Sistema (Pestaña 5):** Consulta `/api/logs` con lectura eficiente y acotada del final de `/data/controld/ctrld.log` (máx. 256 KB, hasta 500 líneas), auto-scroll, selector de líneas, filtro de búsqueda y auto-refresco (5s).
6. **Exportación de Historial a CSV:** Botón `[ 📥 Export CSV ]` en la barra de herramientas del Live Feed que descarga `ctrld-queries-YYYY-MM-DD_HHMM.csv` de forma 100% offline en el navegador.

## 10. Pendientes

- Observar la v3.19.9 unos días.
- Antigravity: publicar la rama `firewalla-v3.19.9-webui` (diff acumulado `/data/controld/patches-v3.19.1-to-v3.19.9.diff`).
- `GUIA_ADMINISTRACION_detallada.md` es de la v3.16 (histórica); esta guía es la vigente.
- Borrar los candidatos `ctrld.v3.19*-candidate` cuando se confirme la estabilidad.
