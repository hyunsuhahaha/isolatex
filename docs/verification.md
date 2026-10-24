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

## 2026-10-24 — 차단기 윈도의 단조 시간

Go 배포본 `src/time/time.go`의 단조 시계 계약을 확인했다. 기존 UnixNano 기반 버킷은 제어된 테스트에서 표시 시각이 뒤로 이동하자 최근 실패 두 개가 집계에서 사라져 실패했다. 버킷을 `time.Since` 경과 시간으로 바꾼 뒤 표시 시각의 후진·전진에도 기록이 유지되고 실제 윈도 경과에만 만료되는 검사가 통과했다. Open 대기와 `open_until` 관측 시각은 기존 Go time.Time 계약을 유지한다.

Windows breaker·gateway 전체 테스트 및 vet, Linux breaker·gateway race 검사가 통과했다. 시각 보정을 위해 개발 머신의 시스템 시각을 변경하지 않았으며 독립된 테스트 시계로 재현했다.

## 2026-10-24 — 알려진 의존성 취약점 제거와 지원 하한 변경

govulncheck v1.9.0으로 Go 1.27.3 Windows·Linux 소스를 검사했다. 변경 전 도달 가능한 취약 함수는 0이었지만 `golang.org/x/sys v0.35.0`의 Windows `NewNTUnicodeString` 취약점 [GO-2026-5024](https://pkg.go.dev/vuln/GO-2026-5024)가 모듈 수준에서 발견됐다. 수정된 v0.44.0으로 올렸으며 이 의존성의 요구 사항에 맞춰 최소 Go를 1.25.0으로 변경했다.

변경 후 Windows·Linux 모두 모듈 수준 검사와 전체 패키지 호출 수준 검사에서 `No vulnerabilities found.`를 출력했다. 실제 재현 명령은 `SECURITY.md`에 있다. Go 1.27.3 Windows 전체 테스트·vet, Linux 전체 race·vet, Go 1.25.0 Linux 전체 테스트, 모듈 체크섬 검증도 통과했다. 컴파일 호환성 하한과 Go upstream의 운영 보안 지원 기간을 구분해 문서화했다.

수정된 의존성으로 다섯 배포 대상 재빌드와 SHA-256 검증을 통과했다. Windows amd64 압축 해제본을 기본 설정으로 실제 실행해 상대 경로 `./bin/demo-app`으로 네 워커가 시작되는 것을 확인했다. 두 방향 장애 smoke에서 각각 보호 요청 41개 성공·PID 유지·장애 워커 재시작이 확인됐으며 그룹별 준비 상태 두 경로는 200이었다. Ctrl+C 후 `workers_stopped` 로그와 모든 워커 PID 종료, 8080·9090·9100–9101·9200–9201 포트 재바인딩 성공을 확인했다. 이 네이티브 Windows 검사는 직접 자식 범위이며 자손 트리 종료를 입증하지 않는다.

CI는 Linux amd64/arm64, Windows amd64, macOS arm64/Intel stable과 Linux/Windows Go 1.25.x 호환성 행으로 확장했다. stable 행과 수동 배포 workflow는 고정한 govulncheck로 포장 전 취약점 검사를 실행한다.

`0cf0332`의 push·PR 실행은 7개 구성, 합계 14개 check가 모두 성공했다. macOS 로그에서 `go1.27.3 darwin/arm64`와 `go1.27.3 darwin/amd64`를, ARM Linux 로그에서 `go1.27.3 linux/arm64`를 확인했다. 각 stable 행의 모듈·호출 수준 취약점 검사는 모두 미검출이었다. Linux 두 아키텍처는 전체 race·양방향 CLI smoke·지연/크래시 비교 실행·네이티브 배포 압축 해제본 검사를 통과했다. macOS 두 아키텍처는 전체 테스트·vet·취약점 검사를 통과했으며 배포 압축 파일 자체의 실행 검사와 구분한다. [원격 CI 실행](https://github.com/hyunsuhahaha/isolatex/actions/runs/39988723571)

## 2026-10-24 — 과거 장애 이후 재시작 대기 초기화

실제 관리형 테스트 프로세스를 종료·복구시키고 재시작 이력의 시계를 2분 진행시키는 검사에서, 기존 구현은 만료된 과거 장애 때문에 다음 대기를 계속 늘렸다. 최근 60초 기록이 모두 사라지면 500ms 기본 지연부터 다시 시작하도록 수정했다. 짧은 간격의 연속 장애에는 지수 증가와 기존 5회 재시작 예산을 유지한다.

새 테스트는 첫 장애의 500–750ms jitter 범위, 바로 다음 장애의 1–1.5s 범위, 이력 만료 후 500–750ms 복귀와 다른 그룹의 Ready·PID·재시작 수 유지를 검사한다. 실제 대기만 제어된 hook으로 관측하며 호스트 시각은 변경하지 않는다. Windows 새 테스트 10회 반복과 전체 supervisor 테스트·vet, Linux supervisor 전체 race·vet가 통과했다.

## 2026-10-24 — 취소 알림 이전의 전체 요청 기한 만료

실제 context 타이머는 한 시간 뒤로 두고 gate의 테스트 시계만 두 시간 전진시켜 타이머 알림이 아직 도착하지 않은 상태를 재현했다. 즉시 입장, 대기열 슬롯 인계, 슬롯 인계 후 재개라는 세 경계에서 기존 코드는 모두 만료 요청에 성공을 반환하거나 유효 대기자보다 먼저 슬롯을 배정했다.

context 오류와 실제 기한을 함께 확인하도록 수정했다. 큐 기한은 인계로 끝나는 기존 계약을 유지하며 전체 요청 기한은 인계 후에도 적용한다. 만료 요청을 건너뛴 직후 다음 유효 대기자가 슬롯을 받고, 인계 후 만료 시 슬롯 누수가 없는 것을 검사했다. 세 회귀 검사와 기존 큐 인계 검사를 Windows에서 100회 반복했고, Windows 전체 테스트·vet와 Linux 전체 race·vet가 통과했다. 호스트 시스템 시각은 변경하지 않았다.

## 2026-10-24 — 실제 CLI의 진행·대기 요청 종료 계약

`TestNativeCLIShutdownWithActiveAndQueuedRequests`는 실제 CLI 실행 파일을 빌드하고 관리형 helper 프로세스 두 개를 실행한다. 파일 장벽으로 alpha 요청을 완료되지 않은 상태로 유지하고, 관리 상태에서 실행 1·대기 1을 확인한 뒤 beta의 독립 요청 성공을 검사한다. SIGTERM 직후 대기 요청은 503 `draining`으로 종료돼야 한다.

정상 유예 시나리오는 워커가 살아 있는 상태에서 장벽을 풀고 기존 PID의 완전한 200 응답을 확인한다. 유예 초과 시나리오는 장벽을 풀지 않고 250ms 유예가 지난 뒤 진행 연결 종료와 `drain_deadline` 로그를 확인한다. 두 경우 모두 정상 런타임 종료·`workers_stopped` 로그·두 워커 PID 소멸·게이트웨이와 관리 포트 및 워커 포트 두 개의 재바인딩을 검사한다.

Linux에서 이 검사를 race 모드로 10회 반복해 두 시나리오가 모두 통과했다. 테스트 실행 파일과 워커 helper에는 race 계측이 적용되지만 별도로 빌드한 CLI는 일반 빌드다. 전체 테스트 명령을 통해 Unix CI에서도 자동 실행되며 Windows에서는 빌드 조건으로 제외한다. 이는 SIGTERM 검사이며 Windows 콘솔 Ctrl+C나 탈출한 자손 정리를 입증하지 않는다.

`68dcc4b`의 push·PR 실행은 7개 구성, 합계 14개 check가 모두 통과했다. Linux amd64/arm64와 macOS arm64/Intel에서 새 Unix 종료 검사를 포함한 전체 테스트가 성공했으며 Windows에서는 기존 전체 테스트·vet·취약점 검사가 통과했다. 이후 기본 브랜치에 병합했다.

## 2026-10-24 — 공개 모듈 설치 경로

Windows Go 1.27.3에서 `go install github.com/hyunsuhahaha/isolatex/cmd/isolatex@15c7638dd5af5225a6aee2f41d363e9604bd84b0`를 별도 GOBIN으로 실행했다. 공개 모듈 버전 `v0.0.0-20261023171426-15c7638dd5af`를 내려받았으며 설치된 실행 파일의 `-version`과 기본 YAML의 `-check`가 성공했다. 브랜치 `@master` 조회는 이보다 이전의 유효한 개발 커밋을 반환했으므로 README는 재현 가능한 설치 예제에 전체 커밋을 고정한다.

Go 모듈 설치는 별도 배포 ldflags·VCS 메타데이터가 없어 `commit`·`built`를 `unknown`으로 출력할 수 있다. 이 경우 모듈 의사 버전에 소스 커밋 접미사가 포함된다. CLI만 설치되며 데모·설정·문서는 포함되지 않는다는 점도 명시했다. 정식 태그나 GitHub 릴리스는 게시하지 않았다.

같은 커밋의 깨끗한 기본 체크아웃에서 `bash scripts/build-dist.sh dev-15c7638 .tools/release-audit-15c7638`로 Linux amd64/arm64·Windows amd64·macOS amd64/arm64의 다섯 묶음을 생성했다. 다섯 SHA-256 검사와 Linux amd64·Windows amd64 실행 파일의 버전·기본 설정 검사가 통과했으며 두 네이티브 실행 파일 모두 전체 커밋 `15c7638dd5af5225a6aee2f41d363e9604bd84b0`을 출력했다. 빌드 중 체크아웃이 변경됐을 가능성이 있는 앞선 `dev-930b401` 묶음은 이 감사의 근거에서 제외했다. ARM Linux·macOS 묶음의 로컬 실행을 주장하지 않는다.

## 2026-10-24 — 크래시 주입의 자동 재전송 방지

기존 비교 도구의 단일 GET 주입은 이미 사용한 연결에서 서버가 요청을 읽고 응답 없이 연결을 닫으면 두 번 실행됐다. 실제 HTTP keep-alive 연결을 준비하고 첫 장애 요청에서 연결을 끊는 회귀 검사가 수정 전에 `one scheduled crash executed 2 times`로 실패했다. Go의 전송 계층 재시도 조건을 확인한 뒤 비교 도구·양방향 smoke의 크래시 주입을 POST로 변경했다. 일반 부하와 지연 부하는 GET을 유지한다.

수정 뒤 자동 재전송 회귀 검사는 Windows에서 100회, Linux race 모드에서 20회 통과했다. 실제 CLI 양방향 smoke에서도 보호 요청 각각 41개 성공·PID 유지와 장애 워커 재시작을 확인했다. 보고서에 크래시 메서드와 장애 대상 풀의 재시작 증가를 기록하며 크래시 시나리오에서 재시작을 관측하지 못하면 실패 처리한다. `sent`는 논리 요청 수이며 TCP 시도나 서버 실행 횟수가 아니다. 이전 GET 원자료는 변경하지 않고 조건을 구분했다.

크래시 요청을 무시하고 항상 200을 반환하는 별도 앱과 실제 CLI를 띄운 음성 대조 검사도 추가했다. 이 앱을 사용한 trial은 보호 요청이 성공해도 장애 실행 미검증 오류로 끝나야 한다. 최종 Windows bench 전체 테스트·vet와 Linux bench 전체 race·vet가 통과했다. 수정 도구로 다시 실행한 POST 크래시 9 trial은 전 단계 생성 누락 0·요청 회계 일치·장애 풀 재시작 증가를 확인했다. C의 장애 단계 보호 요청은 360/360 성공했고 PID가 유지됐다. 새 원자료와 비교표는 `bench/results/README.md`에 있다.

## 2026-10-24 — 준비 상태 응답 헤더의 공유 메모리 노출 제한

관리형·외부 워커가 준비 상태 경로에서 1MiB 헤더와 HTTP 200을 보내는 두 검사에서 기존 Supervisor는 실패 없이 Ready를 유지했다. 준비 검사 transport에 64KiB 헤더 한도를 적용해 초과 응답을 실패로 집계하도록 수정했다. 이 한도는 요청 전달 transport의 기존 헤더 한도와 별개로 감시 경로에도 적용된다.

새 검사는 작은 응답으로 준비 완료 후 큰 헤더 주입·실패 임계치에 따른 제외·작은 응답으로 회복을 확인한다. 관리형 워커의 PID와 재시작 수가 유지되며 다른 그룹의 PID·Ready·건강 검사 진행도 유지된다. Windows 두 회귀 검사 10회 반복과 전체 supervisor 테스트·vet, Linux 전체 supervisor race·vet가 통과했다. 워커 메모리 전체에 대한 상한이나 호스트 OOM 방지를 입증하는 검사는 아니다.

`ce6206a`의 7개 구성, push·PR 합계 14개 원격 check가 모두 통과한 뒤 기본 브랜치에 병합했다.

## 2026-10-24 — Windows 관리형 워커의 자손 수명

이전 직접 자식 종료 방식을 같은 검사에 넣으면 부모만 종료되고 자손 프로세스 핸들이 `WAIT_TIMEOUT(258)`으로 살아 있는 실패를 재현했다. 워커별 KILL_ON_JOB_CLOSE Job과 정지 생성·등록·재개를 도입했다. 구현 중 소유자를 워커 Job 등록 전에 종료하면 정지 자식이 남는 경합도 재현해, 최초 관리형 생성 전 소유자 자신이 속하는 루트 Job을 추가했다. 루트 핸들은 프로세스 수명 동안 하나를 유지하며 OS 종료가 닫는다.

네이티브 Windows 검사는 즉시 자손 생성·해당 트리 종료·별도 워커 유지, 부모 선행 종료 후 Job 닫기, 소유자의 정리 없는 정상 종료와 TerminateProcess, 워커 Job 등록 전 정지 자식 회수를 확인한다. 실제 Supervisor 검사에서는 stdout/stderr를 상속한 자손이 있는 워커를 HTTP POST로 크래시시키고 이전 자손 제거·대체 PID와 자손 생성·다른 그룹의 PID·Ready·재시작 수 유지·Stop 후 회수를 확인했다. 검증과 실패 정리는 재사용 가능한 PID 대신 미리 연 프로세스 핸들을 사용한다.

Windows Go 1.27.3 새 검사 10회 반복과 해당 패키지의 checkptr=2 검사 10회·vet, Go 1.25.0 Windows supervisor 전체 검사가 통과했다. Windows 전체 테스트·vet와 Linux 전체 race·vet도 통과했다. Windows 모듈·호출 수준 govulncheck는 모두 미검출이었다. Windows API로 uintptr를 전달하는 Job 설정 버퍼는 runtime.Pinner로 고정하며 최소 Go 버전과 의존성 버전은 유지했다.

Windows의 종료 범위·Job 중첩 등록 실패·강제 종료 hook 제한·WMI/외부 서비스 자손 제외는 `windows-processes.md`와 SECURITY.md에 명시했다. CPU·메모리 quota 또는 악성 코드 샌드박스를 주장하지 않는다. macOS 등 나머지 OS의 직접 자식 범위는 이 변경으로 확장되지 않는다.

공개 모듈 커밋 `1ffb18ddc45c22cef3c96ba13129f87d98ce3c4e`를 별도 GOBIN에 설치했다. 버전 `v0.0.0-20261024010702-1ffb18ddc45c`와 기본 YAML 검사를 통과한 그 실행 파일을 Windows에서 직접 실행해 네 워커를 준비시켰다. 양방향 smoke에서 보호 요청 각각 41개 성공·PID 유지·장애 워커 재시작이 확인됐다. Ctrl+C 후 `workers_stopped`와 네 PID 종료·8080/9090/9100–9101/9200–9201의 재바인딩을 확인했다.

같은 설치본을 다시 실행해 종료 전부터 런타임의 프로세스 핸들을 별도 관찰자가 유지했다. Ctrl+C 후 실제 CLI 종료 코드는 0이었고 워커·포트가 다시 모두 회수됐다. PowerShell PTY 래퍼는 취소 코드 1을 반환하므로 이를 CLI 종료 코드로 오인하지 않았다. README 공개 설치 스냅샷도 Windows Job 구현이 포함된 이 커밋으로 갱신했다.

`829b409`의 7개 구성, push·PR 합계 14개 원격 check가 모두 통과한 뒤 기본 브랜치에 병합했다. Windows stable·Go 1.25.x의 전체 테스트에 새 Job 검사가 포함됐다.

## 2026-10-24 — macOS 프로세스 그룹과 SIGTERM 무시 자손

Linux의 프로세스 그룹 구현을 macOS에도 적용했다. Linux·Darwin 공통 검사는 직접 부모만 종료하면 자손이 살아 있는 음성 대조와 관리형 그룹의 자손 종료·별도 워커 생존을 비교한다. 정리 완료 뒤 제어 객체는 추가 신호를 차단한다. 그룹을 벗어난 자손과 런타임 SIGKILL의 한계, foreground 실행 계약도 문서화했다.

Linux 전체 race 실행 중 자손이 `/proc` 조회 도중 사라지며 ENOENT 외에 ESRCH를 반환하는 검사 경합을 발견했다. 두 경우를 종료 상태로 처리한 후 그룹 대조 검사가 race 100회를 통과했다. 실제 Supervisor는 SIGTERM을 무시하고 stdout/stderr를 상속한 자손이 있는 워커를 크래시·복구시켜 이전 자손 제거, 새 워커·자손 생성, 다른 그룹의 PID·Ready·HTTP 응답 유지와 최종 Stop 회수를 race 10회 확인했다.

최종 Linux 전체 race·vet와 Go 1.25.0 Windows supervisor 전체 검사가 통과했다. 같은 Go 1.25.0으로 Darwin amd64·arm64 supervisor 테스트 실행 파일의 크로스 컴파일도 통과했다. 로컬에서 macOS 실행을 주장하지 않으며 네이티브 macOS 두 아키텍처의 동작은 원격 CI에서 구분해 확인한다.

첫 원격 Go 1.25.x Linux 검사에서는 Stop 직후 커널 상태를 즉시 읽어 자손 종료 전의 짧은 구간을 실패로 판정했다. 신호 전달의 비동기 계약에 맞춰 최대 3초 안의 실제 종료를 확인하도록 바꾸고 대체 자손의 그룹 ID도 현재 워커와 일치할 때만 추적했다. 종료가 확인된 PID는 실패 정리 대상에서 제거한다. 수정 뒤 Go 1.25.0 Linux의 그룹·SIGTERM 무시 검사 20회가 통과했다.

`108dce4`의 7개 구성, push·PR 합계 14개 check가 모두 성공했다. 원격 로그에서 `go1.27.3 darwin/arm64`와 `go1.27.3 darwin/amd64`를 확인했고 두 네이티브 macOS의 supervisor 전체 검사·vet·모듈/호출 취약점 검사가 통과했다. 이후 기본 브랜치에 병합했다. 같은 공개 커밋을 Windows에서 모듈 버전 `v0.0.0-20261024013028-108dce443a8e`로 설치해 버전·기본 설정 검사를 통과했다.

## 2026-10-24 — 자동 장애 검사의 관리 인스턴스 식별

기존 smoke는 자신이 실행한 프로세스의 준비 상태와 같은 주소에 이미 떠 있는 인스턴스의 응답을 구분하지 않았다. 관리 `/status`에 실제 런타임 PID를 추가하고 smoke driver의 선택적 `-expected-runtime-pid`로 매 관리 snapshot을 확인한다. 자동 스크립트는 자신이 시작한 PID를 전달하며 앱 요청 전과 크래시 주입 직전에 대조한다. JSON 결과에도 대상 런타임 PID를 기록한다.

실제 Gateway·관리 HTTP 서버와 별도 smoke CLI를 사용하는 검사는 식별 없는 대조에서 장애 요청이 실행되는지, 다른 PID를 기대하면 앱 요청이 하나도 실행되지 않는지, 같은 PID를 기대하면 정상 smoke가 완료되는지 확인한다. Windows 검사 5회, Linux 해당 bench·gateway race 5회와 양쪽 vet가 통과했다. 실제 CLI 양방향 smoke도 같은 `runtime_pid`에서 각각 보호 요청 41개 성공·PID 유지·장애 워커 재시작을 확인했다. 기존 수동 인자 생략 동작은 유지하며 gateway/admin URL은 같은 설정에 맞춰야 한다.

`3e7d682`의 7개 구성, push·PR 합계 14개 check가 모두 성공한 뒤 병합했다. 같은 공개 커밋의 모듈 설치가 Windows에서 성공했고 `v0.0.0-20261024015050-3e7d682c0e15` 버전과 기본 설정 검사를 확인했다. README의 설치 스냅샷을 이 커밋으로 갱신했다.

## 2026-10-24 — 네이티브 배포 묶음의 장애 검사

배포 빌드는 GNU `sha256sum` 외에 macOS의 `shasum -a 256`도 선택한다. Linux에서 `sha256sum`을 PATH에서 제외하고 `shasum`만 제공한 별도 도구 경로로 네이티브 묶음을 빌드해 체크섬 검증을 통과했다.

새 `smoke-dist.sh`는 부하 driver만 빌드하고 압축 해제본의 런타임·데모·상대 명령 경로를 그대로 실행한다. Linux amd64에서 체크섬·버전·설정 검사 후 양방향 장애 검사에 각각 보호 요청 41개 성공, 정상 그룹 PID 유지, 장애 워커 재시작이 확인됐다. 관리 PID는 두 실행 모두 574였으며 정상 종료·임시 파일 정리 뒤 성공 표식이 출력됐다. 이 로컬 묶음은 커밋 전 작업 상태여서 `-dirty` 메타데이터를 포함한다.

일반 CI의 stable Linux·macOS 네 행에 같은 네이티브 묶음 실행 검사를 추가했다. macOS 추출본의 실제 통과 여부는 해당 변경의 원격 CI 결과로 별도 확인한다.

## 2026-10-24 — HTTP 상태별·전송 실패별 지연시간

비교 실험 JSON에 HTTP 상태별 건수·전송 대비 비율·P50/P95/P99와 별도 전송 실패 비율·지연시간을 추가했다. 기존 성공 지연시간과 상태 개수는 유지한다. 상태만으로 앱의 503과 게이트웨이의 503을 구분하지 않으며, 200 헤더 뒤 본문이 잘린 응답은 전송 실패로만 집계한다.

Windows compare 전체 검사 5회·vet와 Linux 관련 race 검사 10회가 통과했다. 실제 HTTP 서버의 200·503·500·잘린 200 응답이 각각 올바른 집계에 들어가는지 확인했다. Linux에서 지연·POST 크래시 시나리오를 각각 A/B/C 한 번씩 실행했고 모든 단계·역할에서 상태별 건수와 전송 실패의 합이 sent, 비율 합이 1, 생성 누락이 0이었다. 크래시 실험의 각 trial은 재시작 2회를 관측했고 C의 보호 PID가 유지됐다. 짧은 실행은 보고서 기능 검증이며 성능 결론으로 사용하지 않는다. 기존 원자료는 수정하지 않았다.

`db17922`의 원래 push·PR 두 실행에서 7개 구성, 총 14개 check가 모두 통과한 뒤 배포 검사 변경을 병합했다. 원격 로그에서 Linux amd64/arm64·macOS amd64/arm64 각 네이티브 압축 해제본의 체크섬·버전·설정·양방향 장애 검사와 종료 후 성공 표식을 확인했다. macOS ARM의 runtime_pid는 24172, Intel은 25909였으며 각각 두 방향에서 보호 요청 41개 성공·PID 유지·장애 워커 재시작을 기록했다. 이후 `8fca040`의 14개 check도 모두 통과해 상태별 지연시간 보고서를 병합했다.

## 2026-10-24 — 그룹별 Prometheus 경보 예제

수집 설정·런타임 수집 실패·그룹 준비 워커 소실·그룹별 거부율 경보와 promtool 규칙 테스트를 추가했다. 다른 그룹·다른 인스턴스의 정상 상태, 짧은 실패, 무부하·저부하 조건을 구분한다. 런타임 수집이 실패하면 오래된 그룹 지표로 그룹 경보를 추가 생성하지 않는다. 외부 알림 전송이나 재시작·라우팅 변경은 설정하지 않는다.

공식 Prometheus 3.15.1 Linux amd64 압축 파일을 공식 SHA-256 목록과 대조한 뒤 promtool 설정·규칙 검사를 통과했다. 실제 Prometheus를 9091에, IsolateX를 기본 설정으로 실행했다. 쿼리 API에서 alpha·beta의 준비 워커가 각각 2로 수집됐고 세 경보 규칙의 health가 모두 ok였다. 두 프로세스를 정상 종료했다. 처음 거부율 테스트의 3분 시점은 초기 5분 rate 외삽과 최소 완료율 조건 때문에 아직 pending이었다. 충분한 표본 뒤인 4분 시점을 검사하도록 수정했으며 규칙 임계치를 완화하지 않았다.

예제를 포함한 Linux 네이티브 묶음을 다시 빌드·체크섬 검증·압축 해제했다. 추출본의 Prometheus 설정과 경보 테스트가 통과했고 런타임 양방향 장애 검사도 보호 요청 각각 41개 성공·PID 유지·장애 워커 재시작·정상 종료를 확인했다. CI에는 SHA-256을 고정한 promtool 검사 단계를 추가했다. README의 오래된 '원격 CI 결과 없음' 문구도 현재 검증 범위로 갱신했다.

첫 원격 macOS Intel 검사에서 기존 대형 헤더 회귀 검사가 실패했다. 성공 200을 반환하는 픽스처 파일을 큰 값으로 덮어쓰는 동안 빈/작은 값이 읽힐 수 있는데, 전체 검사 횟수 5회 증가만으로 연속 실패를 판정했던 동기화 결함이었다. Windows에서 파일을 비운 뒤 쓰기를 100ms 지연한 대조가 같은 실패를 재현했다. 연속 실패 임계치·실패 증가량·다른 그룹의 검사 진행을 기다리도록 수정하고, 파일이 빈 상태에서 먼저 5회 검사되는 경계를 테스트에 명시했다. 최종 Windows 관련 검사 20회와 Linux race 관련 검사 20회가 통과했다. 실제 헤더 한도나 준비 상태의 실패 임계치는 변경하지 않았다.

## 2026-10-24 — 정적 upstream 주소의 시작 전 검증

HTTP origin을 검사할 때 SplitHostPort의 구문 분리만 확인해 빈 호스트·빈 포트·0번·65536번·정수 범위를 넘는 포트가 통과했다. 공개 Decode 경로의 여섯 회귀 사례가 기존 구현에서 모두 실패하는 것을 확인했다. 호스트 존재와 숫자 포트 1–65535를 검사하도록 수정했다. 정상 DNS 이름·IPv6·포트 범위 양 끝의 대조는 허용하며, 시작 전 검증이 DNS/실제 연결 가능성을 확인한다는 주장은 하지 않는다. Windows config 전체 검사·vet, Linux config race 검사가 통과했다.

공개 커밋 `3390b02ff7e5b55604e51969660d85a2b0e76f24`를 별도 GOBIN에 설치했고 버전 `v0.0.0-20261024022211-3390b02ff7e5`와 관리형·정적 기본 YAML 검사를 확인했다. 같은 설치본의 실제 `-check`가 여섯 잘못된 upstream 모두에서 그룹별 검증 오류와 종료 코드 1을 반환했다. README 설치 스냅샷을 이 검증된 커밋으로 갱신해 주소 검증 계약과 일치시켰다. Windows 프로세스 제어의 직접 의존성 `golang.org/x/sys/windows`도 설치 안내에 명시했다.

## 2026-10-24 — 선택적 워커 트리 메모리 격벽

관리형 process.resources에 양수 memory_max_bytes를 추가했다. Windows는 워커 Job의 committed memory 합을 제한하고, Linux는 별도 cgroup v2에 memory.max·memory.swap.max=0·memory.oom.group=1을 설정한 뒤 clone3의 cgroup FD로 시작한다. Linux는 위임 부모를 명시하며 미지원 OS·잘못된 설정·권한/컨트롤러 실패에서 제한 없는 실행으로 우회하지 않는다. 설정 한도를 status와 Prometheus gauge에 표시한다.

실제 Supervisor·Gateway·HTTP 워커로 무제한 대조와 제한 검사를 수행했다. 부모 96MiB 할당은 64MiB 예산에서, 부모 80MiB와 자손 176MiB의 합은 트리 256MiB 예산에서 실패·워커 교체로 이어졌다. 같은 할당은 무제한 대조에서 성공했다. Windows는 VirtualAlloc commit으로, Linux는 실제 페이지를 채우는 Go 할당으로 검사했다. 두 OS에서 최종 네 사례를 각각 3회 통과했고 Linux는 race로 실행했다. 다른 그룹의 연속 HTTP 응답·준비 상태·PID와 0회 재시작을 유지했다.

Linux 위임 검사 중 부모 경로만 위임하고 테스트 소유자를 범위 밖에 둔 경우 clone3가 permission denied를 반환하는 것을 확인했다. 소유자를 위임 범위의 manager 자식에 배치한 뒤 일반 계정의 테스트·워커로 모든 검사가 통과했다. 새 CI 도구는 고유 cgroup만 만들고 전역 컨트롤러 정책을 바꾸지 않으며 남은 디렉터리가 있으면 정리에 실패한다. 일반 디렉터리를 cgroup_parent로 지정하면 앱 명령이 실행되지 않는 검사도 포함했다.

Windows 전체 test·vet와 Linux 전체 race·vet가 통과했다. Go 1.25.0 Windows/Linux의 config·supervisor 전체 검사도 통과했으며 같은 최소 버전 Linux의 실제 위임 메모리 race 검사 3회를 통과했다.

Ubuntu/WSL2 Linux 6.6.87·systemd 255에서 실제 CLI를 User=user, Delegate=memory, DelegateSubgroup=manager, OOMPolicy=continue로 실행했다. 자손 메모리 주입 뒤 서비스는 active를 유지했고 런타임 PID가 유지됐다. 보호 워커 PID는 5862 그대로, 장애 워커는 5863에서 5910으로 교체됐으며 재시작 1회였다. 실제 파일 값 268435456·swap 0·OOM 그룹 1과 status의 설정값을 확인한 뒤 임시 서비스를 종료했다. 서비스 예제와 메모리 회계 차이·호스트 여유·미지원 OS·CPU/DB 한계를 문서화했다.

공개 모듈 커밋 eb8466b151c749ad3862abf23081249f44c611fb를 Windows의 별도 GOBIN에 설치했다. 버전 v0.0.0-20261024062105-eb8466b151c7과 기본 YAML·256MiB 메모리 옵션을 추가한 YAML의 실제 -check를 통과했다. README 설치 스냅샷을 이 메모리 기능 포함 커밋으로 갱신했다.

메모리 구현 69f16e6의 7개 구성, push·PR 합계 14개 원격 check가 모두 통과했다. Linux amd64/arm64 및 Go 1.25.x 행은 일반 계정으로 위임 cgroup의 실제 메모리 race 검사도 수행했다. 추가 검토에서 프로세스 그룹을 벗어난 자손의 강제 회수를 별도로 검증했다. setsid 자손이 부모와 같은 cgroup에 남는지 확인하고 강제 종료 후 그 자손 회수·다른 워커 cgroup 생존·디렉터리 제거를 확인한다. 실패 정리는 재사용 가능한 PID 대신 pidfd를 유지한다. 이 검사를 포함한 최소 Go 1.25.0 Linux 위임 race 3회가 통과했다.

추가 검토에서 자원 정리 실패 뒤 재시작을 계속하면 이전 세대의 예산에 새 예산을 겹칠 수 있는 경로를 차단했다. worker_resource_cleanup_failed가 발생한 슬롯은 failed로 멈추고 재시도를 시작하지 않는다. 실제 워커를 HTTP로 크래시시킨 뒤 정리 오류를 주입하는 검사를 추가했다. 이전의 로그 후 재시도 경로는 대조 검사에서 waits=1로 실패했고 수정 후에는 waits=0·슬롯 failed·보호 워커 PID/HTTP 유지가 확인됐다. Windows 검사 5회·Linux race 5회, Windows supervisor 전체·vet 및 최소 Go 1.25.0 Linux의 실제 메모리/cgroup race 검사가 통과했다.

정리 실패 차단까지 포함한 공개 커밋 c3094ed5ef8dc23cc044aedc653a1b15551a5fe5를 별도 Windows GOBIN에 설치했다. v0.0.0-20261024063549-c3094ed5ef8d 버전과 256MiB 메모리 설정의 실제 -check를 통과했으며 설치 스냅샷을 이 커밋으로 갱신했다.


## 2026-10-24 — 워커 태스크·프로세스 생성 격벽

`process.resources.tasks_max`(Linux, 스레드 포함)와 `processes_max`(Windows, 활성 프로세스)를 추가했다. 단위가 다르므로 플랫폼별 필드를 구분하며 잘못된 OS·음수·Windows DWORD 초과·양수 한도 없는 resources는 Decode와 공개 CLI `-check`에서 거부한다. 메모리 한도 없이 생성 수만 제한하거나 기존 메모리 한도와 병용할 수 있다. status와 Prometheus에는 설정 상한을 표시한다.

Linux cgroup에는 요청한 컨트롤러만 설정한다. 생성 전 `pids.max`를 적용하고 기존 cgroup FD로 시작하며, Windows는 정지 워커를 활성 프로세스 한도가 설정된 Job에 등록한 뒤 실행한다. 런타임 소유자 Job은 생성 수를 제한하지 않는다. Linux 컨트롤러 미위임 검사는 앱 실행 흔적과 새 cgroup 누출이 없어야 통과한다.

실제 Supervisor·Gateway와 3세대의 유한한 트리(워커→자식→최대 48개 손자)를 사용했다. 무제한 대조는 모두 생성한다. 생성 수 단독·메모리 병용 제한에서는 손자의 생성이 거부되고, 자손 수거 후 두 번째 요청도 처리한다. 두 그룹에 각각 같은 예산을 설정해도 보호 경로의 연속 HTTP 성공·PID·재시작 0이 유지된다. Windows의 오류를 처리한 격리 워커는 같은 PID로 계속 서비스한다. Linux는 태스크 소진 시 Go 런타임이 종료할 수 있어 해당 워커의 복구도 허용한다. 종료 세대의 cgroup 정리 전에 pids.events를 보존하며, 각 요청에서 커널 거부 횟수 증가를 요구한다. HTTP 502만으로 격리 성공을 판단하지 않는다. 테스트 앱은 자손을 소유한 branch가 비정상 종료하면 스스로 종료해 Supervisor가 트리 전체를 회수하도록 한다. Linux 별도 검사는 `LockOSThread`로 64개 스레드 생성을 시도하며, 제한 시 실제 커널 `pids.events:max`가 증가하고 무제한 대조는 완료되어야 한다. race 빌드의 pthread 실패와 일반 빌드의 Go 런타임 실패 문자열 차이에 의존하지 않는다.

Windows·Linux Go 1.27.3 전체 테스트와 vet, Linux 전체 race를 통과했다. 별도 위임 검사로 메모리·생성 수·스레드·컨트롤러 부재 검사를 race 3회 반복했다. Windows Go 1.25.0에서도 새 설정·실제 생성 수 검사를 통과했다. Linux Go 1.25.0에서도 위임된 메모리·생성 수·스레드·컨트롤러 부재 race 검사를 3회 통과했다. Windows 공개 CLI는 생성 수 단독·메모리 병용 설정을 승인하고 잘못된 플랫폼·오버플로·음수·빈 예산은 exit 1로 거부했다.

음성 대조에서는 Windows 활성 프로세스 한도를 잠시 제거하고 Linux pids 한도를 충분히 크게 바꿨다. 두 환경 모두 48개 생성이 성공하여 회귀 테스트가 `descendant budget not enforced`로 실패했다. 원래 제한을 복원한 뒤 다시 검사했다.

CPU·파일 디스크립터·DB 연결·Windows 스레드 수 상한은 이번 변경 범위가 아니다. 같은 사용자 권한의 앱을 대상으로 하는 운영상 격벽이며 악성 코드 샌드박스가 아니다. [설정과 운영 계약](task-limits.md)을 참고한다.


## 2026-10-24 — 워커·자손 CPU 상한

Linux `process.resources.cpu_millicores`와 Windows `cpu_percent`를 추가했다. Linux는 100ms 주기의 cpu.max를 코어 기준으로 설정하며, Windows는 워커 Job에 ENABLE·HARD_CAP을 적용한다. 두 단위를 혼용하지 않고 플랫폼 불일치·음수·Windows 100 초과·Linux 10m 미만과 quota 계산 오버플로를 Decode에서 거부한다. CPU 단독 또는 메모리·생성 수 한도와 병용할 수 있다. 원래 실행 전 cgroup FD·정지 Job 등록 절차와 실패 시 실행 차단을 유지한다.

실제 Supervisor·Gateway의 부모와 자손이 각각 유한한 시간 동안 CPU를 소모하도록 했다. CPU 무제한 대조·단독·병용을 검사하며, Linux는 같은 cgroup의 정확한 quota·실제 cpu.stat throttling 증가·합산 CPU 시간, Windows는 두 프로세스의 실제 Job hard cap·누적 프로세스 수·합산 CPU 시간을 확인한다. 부하 동안 보호 경로의 연속 HTTP 성공과 두 워커의 PID·재시작 0을 유지한다. 상태 API와 지표의 설정값도 확인했다. 계측 허용 오차를 둔 기능 검사이며 성능 향상율·보호 경로의 SLO를 주장하지 않는다.

Windows Go 1.27.3 전체 테스트·vet, Linux Go 1.27.3 전체 race·vet를 통과했다. Windows Go 1.25.0의 설정·실제 CPU 검사, Linux Go 1.25.0의 별도 위임 CPU race 2회도 통과했다. 기존 메모리·태스크·스레드·컨트롤러 부재 검사를 Linux에서 race 3회 반복해 통과했다. CPU controller 미위임 시 앱 실행 흔적과 새 cgroup이 남지 않는지도 검사한다.

Linux CPU 검사는 systemd가 관리하는 고유한 임시 service에 CPU·메모리·태스크 컨트롤러를 위임해 일반 사용자로 실행한다. 전역 cgroup 컨트롤러 파일을 직접 수정하지 않고 종료 시 정확히 해당 unit을 회수한다. CPU 검사는 별도 scripts/check-cpu-linux.sh와 Linux CI 단계에서 실행하며 일반 테스트의 skip에 의존하지 않는다.

음성 대조에서 Windows CPU 설정을 제거하면 실제 Job flags/rate가 0으로 나와 회귀 검사가 실패했다. Linux cpu.max를 max로 바꾸면 실제 quota 불일치로 실패했다. 두 환경에서 제한을 복원한 뒤 실제 CPU 검사를 다시 통과했다. Windows 공개 CLI -check도 CPU 단독·병용을 승인하고 플랫폼 불일치·음수·100 초과·양수 한도 없는 설정은 exit 1로 거부했다.

CPU는 예약이나 순간 점유율·응답 시간의 보장이 아니다. Linux fair-class 및 Windows 상위 Job 비율 계약, 낮은 한도에 따른 시작·요청 지연, 같은 권한의 악성 앱과 DB·I/O 경합의 한계를 [CPU 운영 문서](cpu-limits.md)에 명시했다.
