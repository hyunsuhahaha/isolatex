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

## 2026-10-24 — 대기열 경합·프로세스 세대·로그 격리·배포

Go 1.27.3 Windows amd64 및 WSL2 Linux amd64에서 검증했다. 이전 커밋 `c05f3d9`의 원격 CI는 Windows stable, Linux stable, Linux Go 1.23.x에서 모두 성공했다. 아래는 그 이후 변경의 로컬 검사다.

수정 전 제어된 시계·잠금으로 두 대기열 문제를 재현했다. 슬롯 반환 시 취소되거나 만료된 맨 앞 요청에 슬롯을 넘기고, 해당 goroutine이 다시 정리할 때까지 유효한 다음 요청을 막았다. 반환 측에서 취소·만료를 확인하도록 수정한 뒤 관련 세 테스트 100회 반복과 전체 gateway 테스트 10회 반복이 통과했다. 슬롯 배정 직후 스케줄링이 늦어져도 끝난 큐 대기 시간을 다시 만료로 처리하지 않는지도 검사했다.

수정 전 새 프로세스가 이전 프로세스의 Active 수를 물려받는 테스트가 실패했다. 프로세스 세대를 도입한 뒤 이전 요청의 완료가 새 워커의 진행 수를 바꾸지 않으며, 동일 PID의 health 회복에서는 진행 수가 유지되는 검사가 통과했다.

로그 저장 대상을 의도적으로 멈춘 실제 HTTP 검사에서 두 그룹 모두 요청이 완료됐고 `isolatex_dropped_log_records_total`이 증가했다. 로그 큐 포화, 동시 생산자, 전달 오류, 레코드 크기 상한, 호출자 버퍼 재사용, 종료 대기 시간 상한도 검사했다. 크래시 루프로 한 그룹이 재시작 5회 후 Failed가 될 때까지 다른 그룹의 요청 성공·PID·Ready 유지도 Windows 및 Linux race 검사로 확인했다.

| 검사 | 결과 |
| --- | --- |
| Windows 전체 테스트·vet·go mod verify | 통과 |
| Linux 전체 race 테스트·vet | 통과 |
| 소스 실행 파일 양방향 장애 smoke | 통과, 방향별 보호 요청 41개 성공 |
| Linux amd64/arm64, Windows amd64, macOS amd64/arm64 배포 빌드 | 다섯 대상 크로스 컴파일 통과 |
| 다섯 압축 파일 SHA-256 검증 | 통과 |
| Linux amd64 압축 해제본 `-version`, `-check` | 통과 |
| Windows amd64 압축 해제본 네이티브 `-version`, `-check` | 통과 |
| Linux amd64 압축 해제본 양방향 장애 smoke | 통과, 방향별 보호 요청 41개 성공 |

패키지 smoke의 보호 요청 P95는 alpha 6.175ms, beta 6.332ms였다. 각 방향에서 보호 대상 PID 유지와 장애 워커 재시작을 확인했고 종료 로그는 `workers_stopped`였다. 패키지에는 당시 커밋되지 않은 소스가 포함되어 `-version`의 커밋에 `-dirty`가 표시됐다. macOS와 arm64 대상은 빌드·체크섬까지만 검증했으며 실제 실행 결과를 주장하지 않는다. 정식 태그나 릴리스는 발행하지 않았다.

## 2026-10-24 — 공개 설치와 A/B/C 비교 도구

`6af64a9`의 원격 CI는 Linux stable·Linux Go 1.23.x·Windows stable에서 모두 성공했다. 공개 모듈의 정확한 커밋으로 `go install github.com/hyunsuhahaha/isolatex/cmd/isolatex@1dd69bc`가 성공했고 설치한 실행 파일은 `-version`에서 `v0.0.0-20261023155615-1dd69bcc737a`를 출력했다. 소스 빌드는 VCS 커밋·dirty 상태를, 배포 빌드는 주입한 메타데이터를 사용한다.

비교 도구의 통계·한도 검사는 Windows 테스트·vet, Linux race·vet를 통과했다. Windows 실제 크래시 비교의 짧은 기능 검사에서 A/B/C의 자원 합계는 각각 앱 4·실행 4·대기 8이었고, 보호 영역 PID는 공유 A/B에서 변경되고 분리 C에서 유지됐다.

Linux에서는 준비 1s·정상 2s·장애 3s·회복 2s, 두 역할 각각 40rps, seed 42로 A/B/C를 세 번씩 비교했다. 지연 시나리오가 끝난 뒤 크래시 시나리오를 실행했다. 모든 trial에서 생성 누락은 0이었고 자원 합계와 요청 수 집계가 일치했다. 각 시나리오의 C 장애 단계에서 보호 요청은 360/360 성공했다. 공유 A/B의 요청 실패와 PID 변경도 원자료에 남겼다. [짧은 실험 기록](../bench/results/README.md)에 반복별 수치·해석·한계를 공개한다. 이는 장기 성능 합격 판정이 아니다.

비교 도구의 실제 지연 실험을 `go run -race ./bench/compare -warmup 0 -baseline 1s -fault 2s -recovery 1s -repeats 1 -healthy-rate 10 -fault-rate 10 -scenario delay -out .tools/compare-race.json`로 실행해 A/B/C 전체 경로의 race 검사도 통과했다. `0b7d253` 원격 CI는 Windows stable·Linux stable·Linux Go 1.23.x 모두 성공했으며 Linux stable에서 새 지연·크래시 비교 실행과 압축 해제본 검사도 통과했다.

## 2026-10-24 — 그룹별 준비 상태

새 실제 HTTP 테스트는 기존 구현에서 `/readyz/normal`이 404라 실패했다. `GET /readyz/{group}`를 추가한 뒤 한 그룹 실패·다른 그룹 유지·반대 그룹 회복·전체 drain·없는 그룹·생존 상태를 Windows gateway 테스트·vet와 Linux gateway race 검사로 검증했다. 전역 `/readyz`의 기존 전체 그룹 집계 동작은 유지한다. 그룹 준비 상태는 워커 존재와 drain 여부이며 포화·차단기·요청 성공 여부와는 별개의 계약이다.
