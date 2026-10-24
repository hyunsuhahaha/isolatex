# IsolateX

장애의 영향 범위를 제한하는 범용 격벽(Bulkhead) 오픈소스. 서로 독립적으로 유지해야 할 작업을 격리 그룹으로 나누고, 한 그룹의 지연·과부하·프로세스 장애가 다른 그룹에 전파되는 것을 줄인다. 그룹의 이름과 역할은 사용자가 정한다.

현재 구현은 Go 기반 단일 호스트 HTTP 런타임으로, **경로별 실행·대기 용량 예약, 독립 프로세스 풀, 그룹 사이 우회 없음**을 제공한다. [프로젝트 목표와 설계](docs/design.md) · [격벽의 보장 범위](docs/bulkheads.md)

| 장애 | 현재 격벽의 역할 |
| --- | --- |
| 지연·과부하 | 해당 그룹의 실행·대기 수를 제한하고 초과 요청 거부 |
| 워커 크래시 | 해당 프로세스를 재시작하고 다른 그룹의 프로세스 유지 |
| 준비 상태 실패 | 해당 워커를 전달 대상에서 제외하고 회복 후 복귀 |
| 반복적인 upstream 실패 | 선택적 Circuit Breaker로 해당 그룹의 전달 차단 |

공유 게이트웨이·호스트·DB는 여전히 장애 전파 경로가 될 수 있다. 현재 보장과 검증 결과는 이 경계를 기준으로 설명한다.

현재 **v0.4 개발 중**이다. 경로별 실행·대기 제한, 프로세스 관리, rolling-window Circuit Breaker, 주기적 Health Check와 복구, Prometheus 카운터·게이지·지연시간 histogram을 구현했다. 자원 조건을 맞춘 비교 벤치마크와 관측 예제를 보완하고 있다. Windows Job Objects는 프로세스 수명 관리에 사용하며 CPU·메모리 상한은 이후 확장 범위다.

## 실행

Go 1.25 이상이 필요하다. 저장소 루트에서 실행한다. HTTP·프로세스 관리는 표준 라이브러리, 설정과 관측에는 YAML 파서와 Prometheus 공식 Go 클라이언트를 사용한다.

Go 1.25는 컴파일 호환성 하한이다. 운영용 실행 파일은 Go 프로젝트가 보안 수정을 지원하는 버전의 최신 패치로 빌드한다. Windows 의존성의 알려진 취약점 수정을 위해 최소 버전을 기존 1.23에서 올렸다. [보안 범위와 검사](SECURITY.md)

Go 모듈 경로는 `github.com/hyunsuhahaha/isolatex`다. 체크아웃한 소스에서 CLI를 설치하려면 `go install ./cmd/isolatex`를 실행한다. 아직 정식 릴리스 태그는 없다. 저장소를 복제하지 않고 CLI만 설치할 때는 검토한 개발 커밋을 고정한다. 다음은 2026-10-24에 공개 모듈 경로에서 설치·버전·설정 검사를 통과한 스냅샷이다.

```sh
go install github.com/hyunsuhahaha/isolatex/cmd/isolatex@15c7638dd5af5225a6aee2f41d363e9604bd84b0
isolatex -version
```

설치 위치는 `GOBIN` 또는 기본 `$GOPATH/bin`이며 해당 경로를 `PATH`에 추가해야 한다. 이 명령은 CLI만 설치한다. 데모 실행 파일과 YAML 예제는 저장소를 복제해 아래 절차로 빌드하거나 배포 묶음을 사용한다.

브랜치·`@latest` 조회가 방금 갱신한 저장소와 다를 수 있으므로 설치 후 `-version`의 모듈 버전과 커밋 접미사를 확인한다. [Go 모듈 미러의 캐시 안내](https://proxy.golang.org/)처럼 변경 사항은 즉시 조회에 반영되지 않을 수 있다. 모듈 설치에서 별도 VCS·배포 메타데이터가 없으면 `commit`과 `built`는 `unknown`이며 소스 식별자는 모듈 버전에 포함된다.

```sh
go build -o bin/demo-app ./examples/demo-app
go build -o bin/isolatex ./cmd/isolatex
./bin/isolatex -config isolatex.yaml -check
./bin/isolatex -config isolatex.yaml
```

Windows PowerShell:

```powershell
go build -o bin/demo-app.exe ./examples/demo-app
go build -o bin/isolatex.exe ./cmd/isolatex
.\bin\isolatex.exe -config isolatex.yaml
```

Linux에서는 워커별 프로세스 그룹에 SIGTERM 후 SIGKILL을 보내고 자식을 수거한다. Windows에서는 워커별 Job Object로 자손 수명을 관리하고, 런타임 소유자 Job이 생성 중 또는 런타임 종료 시 남는 자식을 회수한다. HTTP drain 후 Windows의 Job 종료는 강제 종료이며 앱 종료 hook 실행을 보장하지 않는다. macOS 등 나머지 OS는 직접 자식 종료 범위다. [Windows 프로세스 수명과 검증](docs/windows-processes.md) · CPU·메모리·DB 자원 상한은 강제하지 않는다.

관리 포트는 `127.0.0.1:9090`, 게이트웨이는 `127.0.0.1:8080`이다. Ctrl+C 또는 SIGTERM으로 요청을 drain한 후 워커를 종료한다. `-debug`는 요청별 그룹·결과·소요 시간 로그를 추가한다. 요청 URL, query와 헤더는 런타임 요청 로그에 기록하지 않는다. 워커 stdout/stderr는 그대로 전달되므로 앱 로그의 비밀값은 앱에서 제거해야 한다.

`-version`은 설정이나 워커를 시작하지 않고 버전·소스 커밋·빌드 시간을 출력한다. 로그는 최대 256레코드, 레코드당 64KiB의 비동기 큐를 거쳐 전달한다. 저장 대상이 멈추거나 큐가 넘쳐도 요청과 워커 감시를 기다리게 하지 않는다. 드롭·전달 실패는 `isolatex_dropped_log_records_total`로 집계한다. 종료 시 로그 전달 대기는 최대 250ms이며 전달을 보장하지 않는다.

배포 묶음 생성은 Linux/WSL2에서 `bash scripts/build-dist.sh dev`로 실행한다. Linux amd64/arm64, Windows amd64, macOS amd64/arm64용 실행 파일·데모·설정·문서와 `SHA256SUMS`를 `dist/`에 만든다. GitHub의 수동 `distribution` 워크플로도 동일 스크립트를 사용해 artifact를 생성하며 릴리스를 게시하지 않는다. [배포 검증](docs/distribution.md) · [기여 방법](CONTRIBUTING.md)

## 장애 데모

런타임 실행 후 다른 터미널에서:

```sh
curl http://127.0.0.1:8080/a/work
curl 'http://127.0.0.1:8080/b/work?delay=3s'
curl -X POST 'http://127.0.0.1:8080/b/work?crash=1'
curl http://127.0.0.1:9090/status
go run ./bench
```

Linux에서는 `bash scripts/smoke.sh`로 빌드·시작·준비 확인·검증·종료를 한 번에 수행할 수 있다. 포트 충돌 시 설정을 복사해 포트를 변경하고 `bash scripts/smoke.sh custom.yaml http://127.0.0.1:18080 http://127.0.0.1:19090`처럼 실행한다.

`go run ./bench`는 beta 풀을 포화시키면서 alpha 요청을 보내고, beta 워커 한 개를 종료한 뒤 새 PID로 재시작되는지 확인한다. 실패하면 비정상 종료하며 JSON 결과에 alpha 요청 P95와 beta 응답별 개수를 출력한다. 기능 smoke test이며 성능 비교 실험 결과는 아니다. 데모 앱의 `crash=1`은 실제로 프로세스를 종료하므로 로컬 실험 전용이다.

alpha와 beta는 동등한 격리 그룹의 예시다. 어떤 그룹도 고정된 업무나 정상/장애 역할을 갖지 않는다. 기본 데모는 beta에 장애를 주입하며, 대상은 다음처럼 바꿀 수 있다.

```sh
go run ./bench -healthy-group beta -healthy-path /b/work -fault-group alpha -fault-path /a/work
```

데모 앱은 `/readyz` 이외의 모든 경로에서 `delay`, `crash`, `ignore_cancel`을 지원한다. `DEMO_DELAY`는 기본 지연, `DEMO_MAX_ACTIVE`는 워커 내부 동시 작업 수다. 워커별 `DEMO_FAULT_FILE`에 `{"delay":"2s","status":503}`을 쓰면 그 파일을 사용하는 워커의 요청에 적용한다. `{"ready":false}`로 준비 상태 실패를 주입할 수 있다.

`go run ./bench/compare -scenario delay -out .tools/delay.json`은 런타임과 앱을 직접 시작해 공유 예산·공유 프로세스(A), 분리 예산·공유 프로세스(B), 분리 예산·분리 프로세스(C)를 같은 총 용량에서 비교한다. 기존 런타임이 실행 중일 필요는 없다. `-scenario crash`로 프로세스 장애도 비교한다. 성공 지연시간·응답별 개수·생성 누락을 분리해 저장하며 [실험 조건과 해석](docs/experiments.md)을 함께 확인해야 한다.

## 설정

[isolatex.yaml](isolatex.yaml)은 alpha 워커 2개와 beta 워커 2개를 실행한다. 명령은 YAML 파일 위치가 아닌 **현재 작업 디렉터리**를 기준으로 셸 없이 실행한다. 앱은 런타임이 주입한 `HOST=127.0.0.1`과 `PORT`를 따라 바인딩하고, 설정한 readiness 경로에서 HTTP 200을 반환해야 한다.

- `workers`는 프로세스 개수, `max_concurrent`는 그룹 전체의 전달 중 요청 수다. `queue_size: 0`이면 즉시 수용하거나 거부한다.
- 모든 그룹의 `max_concurrent + queue_size` 합은 `max_admitted_requests` 이하여야 한다. 이 한도는 시작 시 용량 예약을 검증하는 상한이다. 요청마다 전역 슬롯을 선착순으로 경쟁하지 않으며, 그룹의 남는 용량을 다른 그룹이 빌리지 않는다. 기존에 합계보다 작은 전역 한도를 사용했다면 전역 한도를 올리거나 그룹별 한도를 줄여야 한다. 해당 설정은 이제 시작 오류다.
- `/a`와 `/a/work`는 alpha, `/b`와 `/b/work`는 beta다. `/b-old`는 어느 그룹에도 일치하지 않아 404다. 경로는 세그먼트 경계를 포함한 최장 prefix로 분류하며 동일 prefix 중복은 설정 오류다.
- 인코딩된 경로, dot segment, 중복 슬래시 등 모호한 경로는 400으로 거부한다. 초기 버전은 인코딩된 URL 경로가 필요한 API에 적합하지 않다. query는 라우팅에 사용하지 않는다.
- `request_timeout`은 큐 진입부터 응답 본문 전달까지의 예산이다. `queue_timeout`은 그 안의 대기 예산이다. 큐 만료는 503, 전체 예산 만료는 헤더 전송 전 504다. 이미 헤더를 전송했다면 연결을 종료한다. 타임아웃 오류 응답 전달에는 최대 250ms의 추가 쓰기 유예를 준다.
- 슬롯 배정 시 전체 요청 기한도 직접 확인한다. 취소 타이머의 알림 처리가 늦더라도 만료된 대기자를 건너뛰고 다음 유효 요청에 용량을 넘긴다. 대기 예산은 슬롯 배정으로 끝나지만 전체 요청 예산은 계속 적용된다.
- 수용량 초과·준비된 워커 없음·종료 중은 503, upstream 오류는 502, 본문 크기 초과는 413이다. 클라이언트 취소는 upstream에도 전달한다. 앱이 무시하는 작업까지 중단시키지는 못한다.
- 정적 서버를 연결할 때는 `process` 대신 `upstreams`를 사용한다. [예시](examples/static.yaml). 선택적으로 `readiness`를 지정하면 정적 서버도 주기적으로 준비 확인한다. 생략하면 정적 서버를 항상 수용 가능하다고 가정한다. 명시적인 포트를 가진 HTTP origin만 지원한다.
- 다른 그룹으로 우회하거나 애플리케이션 수준의 재시도를 하지 않는다. WebSocket, SSE, CONNECT, gRPC는 지원하지 않는다. HTTPS 종단은 외부 프록시에서 처리한다.
- 알 수 없는 키, 복수 YAML 문서, 중복 prefix, 잘못된 한도·시간, 관리 포트 외부 노출, 관리형 포트 중복, 예약 환경변수 덮어쓰기는 시작 시 거부한다.

전역 기본값: 수용 용량 상한 256, 본문 1MiB, 종료 유예 10초. 기본 예제는 alpha 16자리(실행 8 + 대기 8), beta 6자리(실행 2 + 대기 4), 합계 22자리를 예약한다. 그룹별 실행·대기·시간 설정은 명시해야 한다. 시작 시에는 최대 100ms 간격으로 준비 확인하고, 준비 완료 후에는 `readiness.interval` 주기로 확인한다. 연속 실패 기본 3회에 워커를 제외하고 연속 성공 기본 2회에 같은 PID를 복구한다. `failure_threshold`와 `success_threshold`로 조절할 수 있다. 외부 서비스 장애를 이유로 워커를 강제 재시작하지 않는다.

관리형·외부 upstream의 준비 상태 응답 헤더는 64KiB로 제한한다. 이를 넘는 응답은 HTTP 200이어도 실패로 집계한다. 이미 준비된 워커는 실패 임계치에 따라 제외하고 작은 정상 응답으로 회복하면 같은 PID로 복귀한다. 시작 중 계속 초과하는 관리형 워커는 기존 `startup_timeout`과 재시작 예산을 따른다. 이는 감시 경로의 헤더 한도이며 워커 전체 메모리 상한은 아니다.

그룹의 `circuit_breaker`를 생략하면 차단기를 사용하지 않는다. 설정 예시는 다음과 같다.

```yaml
circuit_breaker:
  window: 10s
  min_requests: 20
  failure_ratio: 0.5
  open_timeout: 5s
```

최근 윈도의 upstream 연결 실패·타임아웃·5xx 비율이 기준 이상이면 Open으로 전환하여 503 `circuit_open`으로 빠르게 거부한다. 큐 포화·로컬 거부·클라이언트 취소는 실패율에서 제외한다. 대기 중에 차단기가 열렸으면 dispatch 직전에 다시 거부한다. 대기 시간이 지난 후 Half-open에서는 단일 요청만 시험하고, 성공하면 Closed, 실패하면 Open으로 돌아간다. 취소된 시험 요청은 다음 시험을 허용한다. 윈도는 고정 100개 시간 버킷을 사용하므로 메모리는 요청량과 무관하며 경계 정밀도는 윈도 길이의 1%다.

실패율 윈도는 시스템 시각 대신 프로세스 내 경과 시간을 사용한다. 시각 동기화로 표시 시각이 앞뒤로 이동해도 최근 실패를 지우거나 다시 포함하지 않는다. 관리 상태의 `open_until`은 관측용 시각이며 실제 대기는 Go의 단조 시계를 사용한다.

재시작은 워커별 500ms부터 최대 30초까지 지수 백오프와 jitter를 적용한다. 최근 60초의 재시작 기록이 모두 만료된 뒤 발생한 새 장애는 500ms 기본값부터 시작한다. 최근 60초에 최대 5회 재시작하고 초과하면 `failed`로 남는다. 복구는 런타임 재실행이다. 포트를 사전 확인하지만 확인과 자식 바인딩 사이에 다른 프로세스가 끼어드는 경쟁까지 제거하지는 않는다.

앱은 중복 실행 가능해야 한다. 메모리 세션·배치·스케줄러·DB 연결 풀 설정을 점검해야 한다. 런타임은 악성 코드를 격리하는 샌드박스가 아니다. 런타임이 SIGKILL로 죽는 경우 자식 정리를 보장하지 않으므로 Linux에서는 서비스 매니저의 전체 그룹 종료 정책을 함께 사용한다.

## 관측과 검증

- `GET /healthz`: 런타임 생존.
- `GET /readyz`: 각 그룹에 준비된 워커가 최소 하나 존재하며 drain 중이 아닌지 확인. 일부 워커가 Failed여도 나머지가 Ready이면 200이다.
- `GET /readyz/{group}`: 해당 그룹에 준비된 워커가 있고 drain 중이 아니면 200, 해당 그룹이 준비되지 않았거나 drain 중이면 503, 없는 그룹이면 404다. 예: `/readyz/alpha`. 다른 그룹의 장애는 이 응답에 영향을 주지 않는다.
- `GET /status`: 그룹별 활성·대기 요청, 차단기 상태, 워커 PID·상태·재시작·Health Check 수. 인증이 없으므로 관리 포트는 loopback에만 바인딩한다.
- `GET /metrics`: 요청·거부 카운터, 실행·대기·준비 워커 게이지, 차단기 상태·전이 수, Health Check·재시작 카운터, 요청·큐 지연시간 histogram과 Go/프로세스 지표.

외부 헬스체크를 경로별로 구성한다면 해당 경로의 그룹 준비 상태를 사용한다. 준비 상태 엔드포인트는 loopback 관리 포트에 있으므로 같은 호스트의 검사 에이전트나 접근 제어된 프록시를 통해 연계한다. 전역 `/readyz`는 모든 그룹을 합친 상태이므로 한 그룹 장애로 전체 게이트웨이를 제외하는 설정이 될 수 있다. 준비 상태는 신규 전달이 가능한 워커의 존재를 뜻하며 포화·Circuit Breaker 상태나 모든 앱 요청의 성공을 보장하지 않는다.

느린 헤더·본문·클라이언트 쓰기에는 서버/요청 deadline을 적용한다. 전역 수용 한도는 분류 이후의 요청만 제한하고 TCP 연결 수 전체를 제한하지 않는다. 외부 트래픽 노출 시 별도의 TLS·인증·연결 보호가 필요하다.

```sh
go test ./... -count=1 -timeout=90s
go test -race ./... -count=1 -timeout=90s
go vet ./...
```

`-race`는 C 컴파일러가 필요하다. 테스트는 실제 HTTP와 자식 프로세스를 사용해 경로 경계, beta 포화, FIFO, 취소와 배정 경합, 타임아웃, 크래시 격리, 재시작, 포트 충돌, 종료 중 재시작 방지를 확인한다. Linux CI에서도 동일 검사를 수행한다.

실제 실행 결과와 검증 환경은 [검증 기록](docs/verification.md)에 정리했다. CI 설정은 추가했으며 원격 CI 실행 결과는 아직 없다.
