# 서버마다 다른 워커 상태로 분산하기

IsolateX는 서버별 로컬 격벽이다. 서버 A의 alpha가 실패해도 A의 beta와 서버 B의 alpha를 유지해야 한다. 서버 전체의 준비 상태만 검사하면 이 구분을 잃으므로 앞단에서도 그룹별 backend 풀을 나눈다.

## 로컬 준비 상태와 분산 후보 상태

| 조회 | 의미 |
| --- | --- |
| 관리 `/healthz` | 런타임 생존 |
| 관리 `/readyz/{group}` | drain 중이 아니며 준비된 워커가 하나 이상 존재 |
| 관리 `/routingz/{group}` | 준비된 워커·차단기·현재 요청 수용 여유를 반영한 분산 후보 상태 |

`/routingz/{group}`는 GET·HEAD 전용이며 200은 현재 수용 가능, 503은 현재 제외 권고, 404는 없는 그룹이다. 예를 들어 실행 슬롯이 모두 사용 중이어도 대기열 자리가 있으면 수용 가능이다. 반환값은 다음 형태이며 `Cache-Control: no-store`를 설정한다.

```json
{"group":"alpha","eligible":true,"reason":"available","ready_workers":2,"active":1,"queued":0,"free_concurrency":1,"free_queue":2}
```

제외 사유는 `draining`, `no_ready_worker`, `circuit_open`, `saturated` 순서로 판단한다. `circuit_open`에는 이미 시험 요청이 진행 중인 Half-open 상태도 포함한다. 준비된 워커가 있어도 실행·대기 자리가 모두 차면 `saturated`다. 이것 때문에 워커 자체를 unhealthy로 바꾸지는 않는다. 조회는 요청 슬롯을 점유하거나 차단기의 시험 요청을 시작하지 않는다.

`free_concurrency`와 `free_queue`는 해당 서버에 설정된 그룹 예산의 현재 여유다. 워커 수에 자동 비례하거나 CPU·DB의 여유를 측정한 값이 아니다. 워커 일부가 사라져도 그룹의 설정 예산 자체는 바뀌지 않는다. 서로 다른 수의 워커를 가진 서버에는 각각 맞는 예산을 설정한다.

## 원격 로드밸런서가 조회하도록 설정

관리 리스너는 기존대로 loopback 전용이다. 원격 로드밸런서를 위해 관리 포트를 외부에 열지 않는다. 대신 각 노드에 다음 항목을 명시하면 공용 데이터 리스너에서 최소한의 분산 후보 상태만 조회할 수 있다.

```yaml
routing_probe_prefix: /_isolatex/route
```

이제 데이터 주소의 `GET /_isolatex/route/alpha`가 관리 `/routingz/alpha`와 같은 결과를 반환한다. 기본값은 비활성화이며 PID·명령·upstream 주소·전체 설정을 노출하지 않는다. 선택한 prefix는 예약 영역이며 앱으로 전달하지 않는다. `/` catch-all 앱과 함께 사용할 수 있지만 다른 구체적인 그룹 경로와 겹치면 설정 오류다. `/a`와 `/a-old`처럼 경계가 다른 경로는 겹치지 않는다.

이 조회에는 인증이 없다. 노드 데이터 주소는 로드밸런서가 접근할 수 있는 사설망에 두고 방화벽으로 접근을 제한한다. 상태에는 용량과 부하 정보가 포함된다. 예제 HAProxy의 외부 frontend는 `/a`, `/b`만 전달하며 조회 경로는 404로 차단한다. 데이터 리스너의 전체 연결 한도가 찬 경우 조회도 대기할 수 있으며, 이는 해당 노드의 여러 그룹에 영향을 줄 수 있는 공용 경계다.

## HAProxy 예제 실행

[노드 설정](../examples/haproxy/node.yaml)과 [HAProxy 설정](../examples/haproxy/haproxy.cfg)을 제공한다. HAProxy 2.8에서 실제 실행을 검증했다.

각 서버에서 demo 앱을 빌드하고, 노드 설정의 `listen`을 해당 서버 사설 IP로 바꾼 뒤 실행한다. `admin_listen`은 loopback으로 유지한다. 두 서버가 서로 다른 호스트라면 같은 워커 포트를 사용해도 된다.

```sh
mkdir -p bin
go build -o bin/demo-app ./examples/demo-app
go run ./cmd/isolatex -config examples/haproxy/node.yaml -check
go run ./cmd/isolatex -config examples/haproxy/node.yaml
```

앞단 호스트에서는 노드 데이터 주소를 지정한다.

```sh
export ISOLATEX_LB_BIND=127.0.0.1:8000
export ISOLATEX_NODE_A=10.0.0.11:8080
export ISOLATEX_NODE_B=10.0.0.12:8080
haproxy -c -f examples/haproxy/haproxy.cfg
haproxy -db -f examples/haproxy/haproxy.cfg
curl http://127.0.0.1:8000/a/work
curl http://127.0.0.1:8000/b/work
```

두 backend는 동일한 노드 주소를 사용하지만 서로 다른 그룹 조회를 검사한다. alpha의 DOWN이 beta의 서버 목록에 전파되지 않는다. 그룹 이름·경로는 모든 노드와 HAProxy에서 일치시켜야 한다. 없는 그룹은 404로 해당 후보에서 제외되며 다른 그룹으로 우회하지 않는다. 이 예제의 `/a`, `/b` 분류는 세그먼트 경계를 지키며, 새로운 경로를 추가할 때도 런타임과 앞단의 분류를 맞춘다.

| 상태 | `/a` 후보 | `/b` 후보 |
| --- | --- | --- |
| A·B 정상 | A, B | A, B |
| A의 alpha만 포화 또는 워커 없음 | B | A, B |
| A·B의 alpha 모두 불가 | 없음 → 503 | A, B |
| A의 alpha만 회복 | A | A, B |

예제는 정상 후보 사이 round-robin이며 반환된 여유 수에 비례하는 동적 가중치나 분산 스케줄러는 구현하지 않는다. 500ms 간격·연속 실패 2회·연속 성공 2회로 변화 감지를 완충한다. 짧은 부하 급증으로 후보가 자주 바뀌면 검사 간격·판정 횟수와 그룹의 대기 예산을 실제 부하에 맞춰 조정한다. 모든 노드를 동시에 제외하는 용량 검사가 필요한 정책인지도 확인한다.

조회와 실제 요청 사이에는 상태가 달라질 수 있다. 표본은 슬롯 예약이 아니며 로컬 격벽이 최종 수용 여부를 결정한다. 초기 HAProxy 시작 시에도 첫 검사 완료 전과 변경 감지 전의 창이 있다. 이때 503이 발생할 수 있다. 전체 서버 여유의 원자적 합산·전역 큐·자동 확장·무중단 성공을 보장하지 않는다. 서버 간 세션·DB·애플리케이션 상태 동기화도 별도 문제다.

예제는 `retries 0`으로 요청을 자동 재실행하지 않으며, 모든 후보가 없으면 503을 반환한다. 변경 요청의 재시도는 앱의 멱등성 정책에 따라 호출자가 결정한다. 앞단 대기도 `timeout queue 100ms`로 제한한다. TLS·인증과 외부 트래픽 정책은 운영 환경에서 추가한다. HAProxy 검사 지시자는 [공식 health-check 문서](https://www.haproxy.com/documentation/haproxy-configuration-tutorials/reliability/health-checks/)와 [2.8 설정 문서](https://docs.haproxy.org/2.8/configuration.html)를 참고한다.

## 재현 검사

```sh
ISOLATEX_TEST_HAPROXY="$(command -v haproxy)" \
  go test -race ./internal/gateway -run '^TestHAProxyIndependentGroupRouting$' -count=2 -timeout=90s
```

검사는 두 노드의 실제 HTTP gateway·Supervisor readiness 감시와 1개/2개의 독립 alpha HTTP upstream을 구성하고 제공된 HAProxy 설정을 직접 실행한다. A의 alpha 실행·대기열 포화 → 회복 → readiness 실패 → 양쪽 alpha 실패 → A만 회복을 순서대로 주입한다. HAProxy의 서버 상태가 그룹별로 수렴하는지와 실제 POST의 목적지를 모두 검사한다. 매 단계 beta는 A·B 모두에서 계속 응답해야 한다. 조회 경로와 `/a-old`가 외부 frontend에서 노출되지 않는지도 확인한다. 이 검사는 OS 워커 생성 검사나 장시간 성능 검사를 대신하지 않으며, 기존 워커 격리 검사는 별도로 실행한다.
