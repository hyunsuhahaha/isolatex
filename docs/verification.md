# v0.2 검증 기록

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
