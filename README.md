# Icecast exporter for Prometheus

[![License](https://img.shields.io/badge/License-Apache%202.0-green.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.24+-00ADD8.svg?logo=go&logoColor=white)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-ready-2496ED.svg?logo=docker&logoColor=white)](Dockerfile)
[![Prometheus](https://img.shields.io/badge/Prometheus-exporter-E6522C.svg?logo=prometheus&logoColor=white)](https://prometheus.io/)
[![Platform](https://img.shields.io/badge/Platform-Linux-FCC624.svg?logo=linux&logoColor=black)](https://www.kernel.org/)

**English** | [Русский](#icecast-exporter-для-prometheus)

A [Prometheus](https://prometheus.io/) exporter for the
[Icecast](https://icecast.org/) streaming media server. Requires the JSON API
(`/status-json.xsl`) provided by Icecast 2.4.0 or newer.

A modernized fork of
[markuslindenberg/icecast_exporter](https://github.com/markuslindenberg/icecast_exporter)
by Markus Lindenberg (Apache License 2.0).

By default the exporter listens on port **9146**.

## What's changed in this fork

- **Fixed the crash** `Can't read JSON: invalid character ' ' in numeric
  literal` — Icecast can emit invalid JSON like `"bitrate": 128 kbps`; the
  response is now sanitized before parsing.
- Modern Go module (Go 1.24+), current dependencies, `log/slog`.
- Handles any form of the `source` field: array, single object, or absent.
- New metrics: total listeners, listener peak, bitrate, source count, server
  info. All original metrics are unchanged.
- Graceful shutdown and a `/healthz` endpoint.
- Multi-stage Docker build with a minimal `scratch` image.
- Unit and integration tests.

## Building

### With Docker

```bash
docker build -t icecast_exporter .
```

### Without Docker

Requires Go 1.24+:

```bash
go build -o icecast_exporter .
go test ./...
```

## Running

### With docker compose

Set your Icecast URL in `docker-compose.yml`, then:

```bash
docker compose up -d
```

The service is limited to 64 MB of RAM, restarts automatically
(`restart: unless-stopped`) and has a built-in healthcheck.

### With docker run

```bash
docker run -d --rm -p 9146:9146 icecast_exporter \
  -icecast.scrape-uri http://192.168.10.1:9804/status-json.xsl
```

Then check `http://localhost:9146/metrics`.

### Flags

```
Usage of ./icecast_exporter:
  -healthcheck
        Check a running exporter instance and exit (for use as a container healthcheck).
  -icecast.scrape-uri string
        URI on which to scrape Icecast. (default "http://localhost:8000/status-json.xsl")
  -icecast.timeout duration
        Timeout for trying to get stats from Icecast. (default 5s)
  -web.listen-address string
        Address to listen on for web interface and telemetry. (default ":9146")
  -web.telemetry-path string
        Path under which to expose metrics. (default "/metrics")
```

### Endpoints

| Path       | Description                          |
| ---------- | ------------------------------------ |
| `/metrics` | Prometheus metrics                   |
| `/healthz` | Liveness check, returns `200 ok`     |
| `/`        | Landing page with a link to metrics  |

## Prometheus configuration

```yaml
scrape_configs:
  - job_name: icecast
    static_configs:
      - targets: ["icecast-exporter:9146"]
```

## Metrics

| Metric | Type | Labels | Description |
| ------ | ---- | ------ | ----------- |
| `icecast_up` | gauge | — | Was the last scrape of Icecast successful (1/0). |
| `icecast_exporter_total_scrapes` | counter | — | Total Icecast scrapes. |
| `icecast_exporter_json_parse_failures` | counter | — | Number of JSON parse errors. |
| `icecast_server_start` | gauge | — | Timestamp (unix) of server startup. |
| `icecast_listeners` | gauge | `listenurl`, `server_type` | Listeners per mount point. |
| `icecast_stream_start` | gauge | `listenurl`, `server_type` | Timestamp (unix) when the source connected. |
| `icecast_listeners_total` | gauge | — | Total listeners across all mount points. *(new)* |
| `icecast_listener_peak` | gauge | `listenurl`, `server_type` | Peak listeners per mount point. *(new)* |
| `icecast_bitrate` | gauge | `listenurl`, `server_type` | Source bitrate (kbps). *(new)* |
| `icecast_source_count` | gauge | — | Number of active sources. *(new)* |
| `icecast_server_info` | gauge | `server_id`, `host`, `location` | Server information; constant `1`. *(new)* |

Fields not reported by the server are not exported.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
Original work Copyright 2016 Markus Lindenberg.

---

# Icecast exporter для Prometheus

[English](#icecast-exporter-for-prometheus) | **Русский**

Экспортер [Prometheus](https://prometheus.io/) для медиасервера потокового
вещания [Icecast](https://icecast.org/). Требуется JSON API
(`/status-json.xsl`), доступный в Icecast 2.4.0 и новее.

Модернизированный форк проекта
[markuslindenberg/icecast_exporter](https://github.com/markuslindenberg/icecast_exporter)
Маркуса Линденберга (Apache License 2.0).

По умолчанию экспортер слушает порт **9146**.

## Что изменено в этом форке

- **Исправлено падение** `Can't read JSON: invalid character ' ' in numeric
  literal` — Icecast может отдавать невалидный JSON вида `"bitrate": 128 kbps`;
  теперь ответ санитизируется перед парсингом.
- Современный Go-модуль (Go 1.24+), актуальные зависимости, `log/slog`.
- Обрабатывается любая форма поля `source`: массив, одиночный объект, отсутствует.
- Новые метрики: общее число слушателей, пик слушателей, битрейт, число
  источников, информация о сервере. Все оригинальные метрики без изменений.
- Корректное завершение работы и endpoint `/healthz`.
- Многоэтапная сборка Docker с минимальным образом на базе `scratch`.
- Юнит- и интеграционные тесты.

## Сборка

### Через Docker

```bash
docker build -t icecast_exporter .
```

### Без Docker

Требуется Go 1.24+:

```bash
go build -o icecast_exporter .
go test ./...
```

## Запуск

### Через docker compose

Укажите URL вашего Icecast в `docker-compose.yml`, затем:

```bash
docker compose up -d
```

Сервис ограничен 64 МБ ОЗУ, перезапускается автоматически
(`restart: unless-stopped`) и имеет встроенный healthcheck.

### Через docker run

```bash
docker run -d --rm -p 9146:9146 icecast_exporter \
  -icecast.scrape-uri http://192.168.10.1:9804/status-json.xsl
```

Затем проверьте `http://localhost:9146/metrics`.

### Флаги

```
Usage of ./icecast_exporter:
  -healthcheck
        Проверить работающий экземпляр экспортера и выйти (для healthcheck контейнера).
  -icecast.scrape-uri string
        URI, с которого снимать статистику Icecast. (по умолчанию "http://localhost:8000/status-json.xsl")
  -icecast.timeout duration
        Таймаут получения статистики с Icecast. (по умолчанию 5s)
  -web.listen-address string
        Адрес, на котором слушать веб-интерфейс и телеметрию. (по умолчанию ":9146")
  -web.telemetry-path string
        Путь, по которому отдаются метрики. (по умолчанию "/metrics")
```

### Endpoints

| Путь       | Описание                             |
| ---------- | ------------------------------------ |
| `/metrics` | Метрики Prometheus                   |
| `/healthz` | Проверка живости, возвращает `200 ok` |
| `/`        | Стартовая страница со ссылкой на метрики |

## Настройка Prometheus

```yaml
scrape_configs:
  - job_name: icecast
    static_configs:
      - targets: ["icecast-exporter:9146"]
```

## Метрики

| Метрика | Тип | Лейблы | Описание |
| ------- | --- | ------ | -------- |
| `icecast_up` | gauge | — | Успешен ли последний опрос Icecast (1/0). |
| `icecast_exporter_total_scrapes` | counter | — | Общее число опросов Icecast. |
| `icecast_exporter_json_parse_failures` | counter | — | Число ошибок парсинга JSON. |
| `icecast_server_start` | gauge | — | Таймстамп (unix) запуска сервера. |
| `icecast_listeners` | gauge | `listenurl`, `server_type` | Слушатели на точку монтирования. |
| `icecast_stream_start` | gauge | `listenurl`, `server_type` | Таймстамп (unix) подключения источника. |
| `icecast_listeners_total` | gauge | — | Слушатели по всем точкам монтирования. *(новая)* |
| `icecast_listener_peak` | gauge | `listenurl`, `server_type` | Пик слушателей на точку монтирования. *(новая)* |
| `icecast_bitrate` | gauge | `listenurl`, `server_type` | Битрейт источника (кбит/с). *(новая)* |
| `icecast_source_count` | gauge | — | Число активных источников. *(новая)* |
| `icecast_server_info` | gauge | `server_id`, `host`, `location` | Информация о сервере; константа `1`. *(новая)* |

Поля, которые сервер не сообщает, не экспортируются.

## Лицензия

Apache License 2.0. См. [LICENSE](LICENSE) и [NOTICE](NOTICE).
Оригинальная работа: Copyright 2016 Markus Lindenberg.
