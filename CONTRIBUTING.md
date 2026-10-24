# 기여하기

IsolateX는 사용자가 정의한 그룹 사이에서 장애의 영향 범위를 줄이는 격벽 오픈소스다. 현재 구현은 단일 호스트 HTTP 요청·프로세스 격리다. 변경은 어떤 장애 전파 경로를 줄이는지와 남는 공유 자원을 설명해야 한다. [설계](docs/design.md)와 [보장 범위](docs/bulkheads.md)를 먼저 확인한다.

## 개발과 검사

Go 1.25 이상을 사용한다. Linux 프로세스 수명 검증에는 Linux 또는 WSL2가 필요하다.

```sh
go test ./... -count=1 -timeout=90s
go test -race ./... -count=1 -timeout=90s
go vet ./...
bash scripts/smoke.sh
```

race 검사는 C 컴파일러가 필요하다. smoke는 실제 자식 프로세스를 실행·종료하며 기본 포트 8080, 9090, 9100–9101, 9200–9201을 사용한다. 기본 예제의 두 그룹을 번갈아 장애 대상으로 삼는다. 개발 머신의 다른 서비스와 포트가 겹치면 설정을 복사하고 `scripts/smoke.sh <config> <gateway-origin> <admin-origin>`에 전달한다.

Unix의 전체 테스트에는 `TestNativeCLIShutdownWithActiveAndQueuedRequests`가 포함된다. 실제 CLI를 빌드하고 동적으로 고른 네 포트에서 관리형 워커 두 개를 실행한다. SIGTERM으로 대기 요청 거부, 진행 요청의 유예 내 완료, 유예 만료 시 강제 연결 종료, 런타임·워커 PID·포트 정리를 검사한다. `go test ./cmd/isolatex -run TestNativeCLIShutdown -count=1 -timeout=60s`로 따로 실행할 수 있다. 이 검사는 Windows 콘솔 신호나 Linux 자손 프로세스 종료 검사를 대신하지 않는다. `-race`는 테스트와 워커 helper를 계측하며, 이 검사에서 별도로 빌드한 CLI는 일반 빌드다.

Windows의 전체 supervisor 테스트에는 실제 Job Object 자손·다른 워커 유지·부모 워커 선행 종료·소유자 강제 종료·등록 전 정지 워커 회수·관리형 워커 크래시 후 자손 회수가 포함된다. `go test ./internal/supervisor -run 'TestWindows(Job|OwnerJob|ManagedCrash)' -count=1 -timeout=60s`로 따로 실행한다. 운영 코드는 프로세스 수명 동안 루트 Job 핸들 하나를 유지하며 워커 세대별 Job은 종료 후 닫는다. [Windows 수명 설계](docs/windows-processes.md)를 변경 시 함께 확인한다.

Linux·macOS의 `TestProcessGroupContainsDescendantsAndPreservesAnotherWorker`는 직접 자식 종료가 자손을 남기는 음성 대조와 관리형 프로세스 그룹의 자손 종료·별도 워커 유지를 같은 검사에서 비교한다. `go test ./internal/supervisor -run TestProcessGroupContains -count=10 -timeout=60s`로 실행한다. macOS는 자손 상태 확인에 기본 `ps`를 사용한다. [프로세스 그룹 경계](docs/process-groups.md)를 함께 확인한다.

## 장애·경합 수정

문제를 일으키는 설정과 장애 순서를 재현하는 테스트를 먼저 작성한다. 임의의 긴 sleep보다 채널·잠금·제어된 시계를 사용해 경합 순서를 고정한다. 취소·만료·종료·재시작 후 실행 슬롯과 프로세스가 회수되는지, 다른 그룹의 용량과 PID가 유지되는지 확인한다.

부하 검사에서 성공·거절·실패를 나눠 기록하고 총 프로세스 수·실행/대기 예산·실제 전송 부하를 함께 제공한다. 크로스 컴파일, 기능 smoke, 성능 비교, 실제 OS 실행 검증을 구분한다. 재현 명령·환경·실제 결과는 `docs/verification.md`에 남긴다.

## PR 설명

문제의 트리거, 변경 후 격리 동작, 검증 결과, 설정·CLI·메트릭의 호환성 영향을 적는다. 새 옵션을 추가할 때는 기본값, 검증 규칙, 실패 시 동작을 문서화한다. 그룹명·사용자 URL·PID를 제한 없이 메트릭 label로 추가하지 않는다. 업무별 예제는 범용 격리 계약을 검증하는 시나리오로 다룬다.

## 배포

배포 묶음은 `bash scripts/build-dist.sh dev`로 재현한다. 생성된 실행 파일과 도구는 커밋하지 않는다. 정식 태그·릴리스 발행은 유지 관리자가 검증 기록과 호환성 영향을 검토한 뒤 진행한다.

Linux/macOS에서는 압축을 푼 네이티브 묶음을 `bash scripts/smoke-dist.sh <추출 폴더>`로 검사한다. Go·curl과 기본 설정의 빈 포트가 필요하다. CI stable Unix 행도 이 경로로 실제 묶음의 런타임·워커를 실행한다. 절차는 [배포 검증](docs/distribution.md)을 따른다.
