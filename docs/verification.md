# v0.2 검증 기록

> 아래 초기 기록의 업무별 명칭과 경로는 당시 데모 예제다. 현재 프로젝트의 범위와 격리 계약은 [설계](design.md)에 정의한다. 과거 실측 결과는 원래 출력 그대로 보존한다.

검증일: 2026-10-23. Go 1.27.3, Windows amd64 및 Ubuntu on WSL2 Linux amd64. 공식 Go 배포본을 받아 SHA-256을 확인한 후 작업 폴더 전용 `.tools`에 설치했다. 도구와 빌드 결과물은 Git에서 제외한다.

## 자동 검사

| 검사 | 결과 |
| --- | --- |
| Windows `go test ./... -count=1 -timeout=90s` | 통과 |
| Windows `go vet ./...` | 통과 |
| Windows Gateway 테스트 10회 반복 | 통과 |
| Windows 런타임·데모 앱·smoke 실행 파일 빌드 | 통과 |
| YAML 설정 `-check` | 통과 |
| Linux `go test -race ./... -count=1 -timeout=90s` | 통과 |
| Linux 프로세스 그룹 자손 종료 테스트 `-race` | 통과 |
| Linux `go vet ./...` | 통과 |

검증 항목: ERP 포화 시 정상 경로 수용, FIFO, 취소/배정 경합과 슬롯 회수, 전역 수용 한도, 크기 미상 본문 제한, 경로 모호성 거부, 전달 헤더 위조 제거, 준비된 워커 없음, 관리 API, drain, 실제 자식 크래시·재시작·정상 풀 유지, 시작 준비 시간 제한, 포트 충돌, 종료 중 재시작 금지, 반복 실패 5회 후 Failed, Linux 자손 종료.

Linux race 검사 도중 전체 요청 마감 시 빈 200이 나올 수 있는 문제를 발견해 수정했다. 서버의 읽기 deadline이 부모 context를 취소하는 경우와 upstream 응답이 마감과 동시에 도착하는 경우를 실제 마감 시간으로 판정한다. 이후 전체 race 검사와 Windows Gateway 10회 반복 검사가 통과했다.

## 실행 파일을 이용한 Linux 장애 데모

기존 서비스가 8080을 사용하고 있어 검증용 설정에서 Gateway 18080, 관리 19090, 일반 워커 19100~19101, ERP 워커 19200~19201을 사용했다. 저장소의 기본 예제 설정은 변경하지 않았다.

```sh
bash scripts/smoke.sh .tools/smoke.yaml http://127.0.0.1:18080 http://127.0.0.1:19090
```

결과:

```json
{
  "erp_status_counts": {"200": 2, "503": 10},
  "erp_worker_restarted": true,
  "normal_p95_ms": 3.424,
  "normal_pids_unchanged": true,
  "normal_requests": 41,
  "result": "passed"
}
```

과부하 구간의 일반 요청 40개와 크래시 후 요청 1개가 성공했다. P95는 과부하 구간 40개의 성공 요청에서 계산했다. ERP 워커 하나를 실제 종료한 뒤 새 PID로 복구됐고 일반 워커 PID는 유지됐다. SIGTERM 종료 후 `workers_stopped` 로그와 남은 런타임/데모 프로세스가 없음을 확인했다.

이는 로컬 기능 검사다. 공유 풀 대비 성능 개선율, CPU·DB 격리, 대규모 부하나 운영 안정성을 입증하는 결과가 아니다. 설계 문서의 A/B/C 비교 실험은 아직 수행하지 않았다.

Windows의 별도 백그라운드 CLI 실행·프로세스 정리 명령은 자동 승인 검토에서 차단됐고 세부 이유는 제공되지 않았다. Windows에서는 빌드와 실제 자식 프로세스를 사용하는 통합 테스트까지 검증했고, CLI 전체 실행 smoke는 Linux에서 검증했다.

## 2026-10-24 — 격벽 용량 예약

Go 1.27.3, Windows amd64 및 WSL2 Ubuntu Linux amd64에서 검증했다. 각 OS의 공식 배포본 SHA-256을 확인하고 저장소의 Git 제외 폴더 `.tools`에서 실행했다.

| 검사 | 결과 |
| --- | --- |
| Windows `go test ./... -count=1 -timeout=90s` | 통과 |
| Windows `go vet ./...` | 통과 |
| Windows 격벽 포화·예약·FIFO/취소 경합 테스트 10회 | 통과 |
| 기본 설정 `-check` | 통과 |
| Linux `go test -race ./... -count=1 -timeout=90s` | 통과 |
| Linux `go vet ./...` | 통과 |
| Linux `bash scripts/smoke.sh` | 통과 |

수정 전에는 새 설정 테스트 중 부족한 단일 그룹 예산, 여러 그룹 합계 초과, 기본 상한 초과, 정수 오버플로 네 경우가 모두 오류 없이 수용되어 실패했다. 수정 후에는 모두 시작 시 거부한다. 실제 HTTP 테스트는 실행 1 + 대기 1인 두 그룹을 전역 상한 4와 8에서 각각 검증했다. ERP 두 자리를 채운 뒤 초과 요청 32개를 보내는 동안 일반 그룹의 두 자리도 수용되고, 초과 요청은 `queue_full`로 거부되며, 완료 후 모든 슬롯이 회수됐다.

CLI 장애 데모 결과:

```json
{
  "erp_status_counts": {"200": 2, "503": 10},
  "erp_worker_restarted": true,
  "normal_p95_ms": 6.79,
  "normal_pids_unchanged": true,
  "normal_requests": 41,
  "result": "passed"
}
```

종료 로그에서 `workers_stopped`를 확인했다. P95는 이번 로컬 기능 검사에서 관측한 값이며 기존 수치와의 성능 비교 결과가 아니다. 이번 변경은 경로별 요청·프로세스 격리를 대상으로 하며 CPU·메모리의 OS 자원 상한은 추가하지 않았다.

## 2026-10-24 — 범용 장애 주입과 양방향 격리

데모를 임의 경로·워커에 지연, 오류 응답, 취소 무시, 준비 상태 실패, 크래시를 주입하는 방식으로 변경했다. 업무 전용 경로 조건을 제거하고 `DEMO_DELAY`, fault JSON의 `delay`·`status`로 통일했다. 기본 예제 그룹은 `/a`의 alpha와 `/b`의 beta이며, 검사 도구는 `-healthy-group`, `-fault-group`, `-healthy-path`, `-fault-path`로 대상을 지정한다.

Windows 전체 테스트와 vet, Linux 전체 race 테스트와 vet가 통과했다. Linux `bash scripts/smoke.sh`는 두 그룹을 번갈아 장애 대상으로 삼아 다음 결과를 출력했다.

```json
{"fault_group":"beta","fault_status_counts":{"200":2,"503":8},"fault_worker_restarted":true,"healthy_group":"alpha","healthy_p95_ms":6.526,"healthy_pids_unchanged":true,"healthy_requests":41,"result":"passed"}
{"fault_group":"alpha","fault_status_counts":{"200":2,"503":12,"504":6},"fault_worker_restarted":true,"healthy_group":"beta","healthy_p95_ms":6.859,"healthy_pids_unchanged":true,"healthy_requests":41,"result":"passed"}
```

양쪽 모두 보호 대상 그룹의 요청 41개가 성공하고 PID가 유지됐으며 장애를 주입한 그룹의 워커만 재시작됐다. alpha는 실행 예산 8에 워커 2개이고 데모 앱 내부 작업 한도는 워커당 1이므로 지연 주입 시 일부 요청은 요청 예산을 소진해 504가 됐다. 이 실패가 beta에 전파되지 않았는지 함께 검사했다. 수치는 이번 로컬 기능 검사 결과이며 성능 비교 결과가 아니다.
