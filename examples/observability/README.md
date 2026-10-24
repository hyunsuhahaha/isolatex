# 그룹별 관측 예제

같은 호스트에서 실행하는 Prometheus용 시작 예제다. 기본 IsolateX 설정의 관리 포트 `127.0.0.1:9090`을 수집한다. Prometheus 자체는 다른 포트로 실행한다. 바이너리는 [공식 다운로드](https://prometheus.io/download/)에서 별도로 설치한다.

저장소 또는 배포 묶음의 루트에서:

```sh
promtool check config examples/observability/prometheus.yml
promtool test rules examples/observability/alerts.test.yml
prometheus --config.file=examples/observability/prometheus.yml --web.listen-address=127.0.0.1:9091 --storage.tsdb.path=.tools/prometheus-data
```

`http://127.0.0.1:9091`에서 Targets·Alerts·쿼리 결과를 확인한다. 관리 포트는 loopback을 유지한다. 컨테이너나 원격 Prometheus에서 수집할 때의 주소·접근 제어는 해당 환경에 맞춰 설정해야 한다. 이 예제에는 Alertmanager나 외부 알림 전송 설정이 없다.

| 경보 | 기본 조건 | 범위 |
| --- | --- | --- |
| `IsolateXRuntimeUnreachable` | 수집 실패가 1분 지속 | 런타임 instance |
| `IsolateXGroupUnavailable` | 준비 워커 0이 30초 지속 | instance·group |
| `IsolateXGroupRejections` | 5분 윈도의 로컬 거부 비율 10% 초과·완료율 초당 1 이상이 2분 지속 | instance·group |

임계치는 시작 예시이며 실제 부하·시간 예산·오류 목표에 맞춰 조정한다. `rate`는 수집 초기의 표본 수와 외삽 영향을 받으므로 `for` 시간만으로 최초 경보 시점을 예측하지 않는다. 준비 워커가 있어도 포화나 열린 차단기로 요청이 거부될 수 있다. `up=0` 동안 그룹 경보는 억제하고 런타임 경보로 구분한다. 수집 대상이 설정에서 삭제되어 `up` 시계열 자체가 사라지는 경우는 외부 대상 목록 감시가 필요하다.

이 경보는 상태를 알릴 뿐 라우팅·재시작을 변경하지 않는다. 한 그룹의 장애 때문에 전체 게이트웨이를 제거하는 동작으로 연결하지 않는다. 경로별 헬스체크는 해당 `/readyz/{group}`에 연결한다.

## 쿼리

실행 슬롯 사용률(0~1):

```promql
isolatex_active_requests{job="isolatex"} / isolatex_concurrent_limit{job="isolatex"}
```

대기열 깊이는 `isolatex_queued_requests`와 `isolatex_queue_limit`를 함께 읽는다. `queue_size: 0`도 유효하므로 대기 한도로 무조건 나누지 않는다.

결과별 완료율:

```promql
sum by (instance, group, outcome) (rate(isolatex_requests_total{job="isolatex"}[5m]))
```

성공 결과의 요청 지연시간 P95(큐·응답 전달 포함, 초 단위):

```promql
histogram_quantile(0.95, sum by (instance, group, le) (rate(isolatex_request_duration_seconds_bucket{job="isolatex",outcome="success"}[5m])))
```

거부 응답이 빨라져 성공 지연시간이 좋아진 것처럼 보이지 않도록 결과별 비율과 함께 읽는다. 아직 발생하지 않은 outcome 카운터·히스토그램 시계열은 없을 수 있고 표본이 없으면 지연시간도 의미가 없다. 요청 카운터는 경로가 분류된 요청의 완료 시점에 기록하므로 TCP 연결 수나 진행 요청 수와 다르다.

워커 재시작과 런타임 로그 유실:

```promql
sum by (instance, group) (increase(isolatex_worker_restarts_total{job="isolatex"}[5m]))
increase(isolatex_dropped_log_records_total{job="isolatex"}[5m])
```

로그 유실은 런타임 전체 지표다. 워커 프로세스의 CPU·메모리 사용량은 이 런타임 지표에 포함되지 않으며 호스트/프로세스 관측 도구가 필요하다. `isolatex_worker_memory_limit_bytes`는 Linux·Windows의 선택적 워커당 설정값이며 실제 사용량이 아니다. [메모리 예산](../../docs/memory-limits.md)과 CPU 한도를 구분한다.

## 규칙 검사

[Prometheus 규칙 테스트](https://prometheus.io/docs/prometheus/latest/configuration/unit_testing_rules/)로 한 그룹의 장애와 다른 그룹의 생존, 런타임 수집 실패, 그룹별 거부 비율, 무부하와 짧은 준비 상태 실패를 검사한다. CI는 SHA-256을 고정한 promtool 3.15.1을 사용한다. 개발 환경에서는 `bash scripts/check-observability.sh /path/to/promtool`로 동일 검사를 실행할 수 있다.
