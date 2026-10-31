# 수용 TCP 연결 경계

그룹별 실행 슬롯·대기열은 HTTP 경로를 읽은 뒤 적용된다. 헤더를 보내지 않는 클라이언트와 keep-alive 연결도 런타임의 FD·소켓·HTTP goroutine을 사용하므로 별도 연결 상한이 필요하다.

```yaml
max_connections: 1024
```

공용 리스너가 수용한 미종료 TCP 연결의 상한이다. 생략 시 1024이며 양의 YAML 정수만 허용한다. 실행 중인 요청, 요청 본문 수신, 헤더 수신 전, keep-alive 유휴 연결을 모두 센다. HTTP 요청 수와 같지 않으며 그룹별 요청 용량 합계보다 작거나 클 수 있다. 워커로 향하는 upstream 연결은 이 수에 포함하지 않는다.

관리 리스너는 공용 예산과 독립적인 고정 64개 상한을 사용한다. 공용 연결이 포화돼도 관리 포트의 연결 예산은 남는다. 관리 포트 자체가 64개로 포화되면 새 관리 연결도 대기한다. loopback 전용·인증 없음이라는 기존 접근 계약을 바꾸지 않는다.

한도가 가득 차면 런타임은 새 연결의 Accept를 대기한다. 추가 소켓은 커널의 listen backlog에서 기다릴 수 있어 TCP connect가 성공해도 HTTP 응답이 즉시 오지 않을 수 있다. backlog도 가득 차면 OS·네트워크 정책에 따라 연결이 지연되거나 실패한다. 경로를 읽기 전이므로 이 단계에서 HTTP 503이나 그룹별 거절 지표를 반환하지 않는다.

연결이 닫히면 한 슬롯을 회수한다. 리스너 종료는 OS Accept와 예산 대기를 모두 해제한다. 이미 수용한 연결은 HTTP 서버가 기존 graceful shutdown 정책에 따라 처리하며, 연결 래퍼가 미리 끊지 않는다. 헤더 수신 제한 5초·유휴 제한 30초와 요청별 기한도 함께 적용된다.

## 응답을 읽지 않는 클라이언트

공용 HTTP 서버는 그룹 요청 기한을 최종 응답 전송에도 적용한다. 작은 `Content-Length` 응답도 마지막 버퍼를 전송한 뒤 실행 슬롯과 upstream 선택을 반환한다. 클라이언트가 응답을 읽지 않아 전송이 막히면 기한에 따라 연결이 닫히고 수용 연결 예산이 회수된다. 이때 전송 실패는 `isolatex_requests_total`의 `timeout` 또는 `stream_error`로 집계하며, 정상 upstream의 회로 차단기를 실패로 처리하지 않는다.

경로 미일치·drain 거절·공용 라우팅 점검 같은 로컬 응답은 250ms 쓰기 기한을 적용한다. 그룹 대기 단계는 요청 기한에 250ms 오류 응답 여유를 더하고, upstream으로 전달하면 그룹 요청 기한으로 줄인다. upstream 시간 초과 오류 응답에도 250ms 전송 여유가 있다. HTTP 서버가 응답 마무리를 끝낸 뒤 기한을 해제하므로 keep-alive의 다음 요청은 자신의 새 기한을 사용한다. 이 쓰기 기한은 요청 헤더·본문 수신 제한을 대신하지 않는다.

이 동작은 실행 파일의 `net/http` 서버 기준이다. Gateway를 별도 HTTP 서버에 포함하거나 응답 writer로 감싸면 `http.ResponseController`의 기한 설정과 flush 전달을 지원해야 같은 연결 회수 계약이 성립한다. 이미 전송된 HTTP 200을 실패 코드로 바꿀 수는 없으므로 부분 응답·연결 종료와 서버의 결과 지표를 함께 확인한다.

## 관측과 용량 설정

관리 포트의 `/metrics`에 다음 gauge가 있다. `listener`는 `public` 또는 `admin`이다.

- `isolatex_connections`: 현재 수용한 미종료 TCP 연결 수. 관측 중인 관리 연결 자체도 포함한다.
- `isolatex_connections_limit`: 해당 리스너의 수용 상한.

한도에 근접한 연결 수와 낮은 그룹 활성 요청 수가 함께 나타나면 헤더 수신 전 연결·유휴 연결·클라이언트 응답 전송 상태를 외부 프록시에서도 확인한다. 측정값은 소켓 수이며 호스트 FD 총량이나 backlog 점유량이 아니다.

OS FD 상한에는 공용·관리 리스너, upstream 연결, 워커 파이프, 로그와 기타 파일도 포함해야 한다. `max_connections`를 FD 상한과 같게 설정하면 관리·upstream에 필요한 여유를 보장할 수 없다. 운영 환경의 FD 한도와 실제 부하를 보고 공용 상한을 정한다. 기본 1024는 모든 호스트에서 안전하다는 보장이 아니다.

## 남는 공유 경계

경로 분류 전에는 그룹을 알 수 없으므로 공용 연결 예산은 모든 경로가 공유한다. 한 그룹의 클라이언트가 전부 점유하면 다른 그룹의 새 TCP 연결도 대기한다. 이 상한은 런타임이 수용하는 소켓 수를 제한하며 경로별 연결 예약, 클라이언트별 공정성, 호스트 전체 FD 고갈 방지, SYN flood 방어를 제공하지 않는다. 필요하면 앞단 프록시에서 연결·속도를 제한하거나 서로 다른 런타임·리스너로 네트워크 경계를 분리한다.

## 검증

실제 CLI를 실행하고 공용 상한을 2로 설정한다. 헤더를 보내지 않는 연결 하나와 응답 후 keep-alive 연결 하나로 포화시킨 뒤 세 번째 요청이 응답을 받지 않고 기다리는지 확인한다. 그동안 관리 `/healthz`, `/readyz`, `/status`, `/metrics`는 응답해야 한다. keep-alive 연결을 닫으면 대기 요청이 200으로 완료되고, 전부 닫으면 공용 연결 gauge가 0으로 돌아와야 한다. 상한을 한 개 늘린 대조 구현에서는 초과 요청이 처리돼 이 검사가 실패한다.

별도 리스너 검사는 상한 대기 중 Close가 Accept를 해제하고, 이미 수용한 연결은 계속 읽을 수 있으며, 중복 연결 Close가 예산을 두 번 반환하지 않는지 확인한다.

응답 미수신 검사는 수용 연결 상한 1개와 작은 TCP 송수신 버퍼를 사용한다. 클라이언트가 작은 응답을 연속 요청하고 읽지 않는 상태를 만들고, upstream 응답·경로 미일치·라우팅 점검·준비 워커 없음 각각에서 연결 종료와 예산 회수를 세 차례 반복한다. 작은 로컬 응답 조건은 검사 전용 32KiB 헤더로 소켓 버퍼를 빨리 채우며 실제 본문과 gateway 경로는 유지한다. upstream 조건은 원래의 작은 Content-Length 응답을 그대로 사용한다. 매번 다른 정상 경로의 200 응답, 실행·대기 슬롯 0, upstream 회로 유지도 확인한다. 수정 전 upstream 응답 검사는 3초 후에도 연결 1개가 점유된 채 실패했다.

## Upstream 연결 시도의 요청 기한

HTTP Transport는 연결 재사용을 위해 요청 취소와 별개로 연결 시도를 계속할 수 있다. IsolateX는 경로별 요청 기한과 취소를 연결 시도에도 전달한다. 요청이 시간 초과되거나 클라이언트가 취소하면 해당 요청이 시작한 미완료 DNS/TCP 연결 시도도 취소한다. 전송 계층 자체의 취소도 유지한다. 연결 시도가 정상 완료되면 연결은 기존 keep-alive 정책대로 재사용한다. 끝난 요청이 시작한 연결 시도를 다음 요청을 위한 예열 용도로 계속 유지하지 않는다.

기존 연결 시도 상한 시간 3초와 그룹 request_timeout을 함께 적용한다. 요청의 남은 시간이 더 짧으면 그 기한이 우선한다. 요청 시간 초과 응답은 기존 504를 유지하고, 클라이언트 취소는 정상 upstream의 차단기 실패로 세지 않는다. 기한 초과·취소 오류 응답은 HTTP 연결을 닫아 취소된 서버 연결 문맥을 다음 keep-alive 요청에 재사용하지 않는다. 정상 응답의 keep-alive는 유지한다.

공용 max_connections는 downstream 수용 연결 한도다. upstream 연결의 그룹별 MaxConnsPerHost는 max_concurrent이며 유휴 연결의 그룹 전체 상한도 max_concurrent다. 여러 워커 주소의 진행 중 연결 시도 전체를 max_concurrent 하나로 제한하는 별도 전역 세마포어는 없다. 이번 정책은 끝난 요청의 연결 시도가 남는 시간을 줄이며 호스트 전체 FD 예약이나 DNS resolver의 내부 OS 작업까지 즉시 회수한다는 보장은 아니다. 별도 서버에 Gateway를 포함해 커스텀 dial 함수를 사용할 경우에도 context 취소를 따라야 한다.

회귀 검사는 네 개의 실제 TCP 대상에 소켓을 연결한 뒤 dial 완료를 의도적으로 지연시킨다. 요청 기한이 끝나도 연결 시도가 계속되는 대조 구현에서는 활성 4개가 남았다. 수정 구현은 요청 기한과 클라이언트 취소 각각에서 연결 시도를 회수하고 다른 경로의 HTTP 200·요청 슬롯 회수를 확인한다. 별도로 시간 초과 응답의 Connection: close와 이후 정상 요청을 반복 검사한다.

## 요청 본문 시간 초과와 차단기

그룹 request_timeout에는 클라이언트 본문을 받는 시간도 포함된다. Gateway가 본문 읽기에서 실제 timeout 오류를 관측하고 전체 요청도 시간 초과로 끝난 경우 `isolatex_requests_total`과 요청 지연시간의 outcome을 `request_body_timeout`으로 기록한다. HTTP 응답은 기존 504/timeout을 유지한다. 이 결과는 upstream 차단기의 성공·실패 표본에서 모두 제외하므로 본문을 보내다 멈춘 클라이언트가 정상 upstream의 차단기를 여는 것을 막는다. Half-open 시험의 점유도 기존 Ignored 완료 정책에 따라 반환한다.

본문이 모두 수신된 뒤 upstream이 응답하지 않는 시간 초과는 기존 실패 표본이다. 실제 upstream 5xx가 확인된 요청도 그 뒤의 본문 읽기 시간 초과 때문에 실패 증거를 지우지 않는다. 본문이 덜 왔다는 추측만으로 분류하지 않고 본문 reader가 반환한 timeout을 확인한다. 이 구분은 요청 예산이나 연결 종료를 늦추지 않으며 실행·대기 슬롯 회수 정책도 유지한다.

운영 시 `request_body_timeout` 증가는 앞단 프록시·업로드 클라이언트·전송 중단을 함께 확인한다. 기존 `timeout` 및 `upstream_5xx`와 분리해 살펴보고 한 그룹의 모든 시간 초과를 backend 장애로 합산하지 않는다.

## 잘못된 요청 본문과 차단기

chunked 크기·구분자·trailer 오류처럼 클라이언트 본문 reader가 반환한 오류가 Transport 오류로 전달되면 HTTP 400 / `request_body_error`로 응답하고 연결을 닫는다. 해당 결과는 차단기 성공·실패 표본에서 제외한다. 전송 계층이 upstream 장애 때문에 본문을 닫은 `http.ErrBodyReadAfterClose`는 클라이언트 오류 증거로 취급하지 않는다. upstream 연결 오류를 본문이 미완료라는 이유만으로 무시하지 않는다.

본문 최대 크기 초과의 413, 요청 기한 초과의 504, 클라이언트 취소 정책은 우선 적용한다. 이미 upstream 응답을 전달하기 시작한 경우에는 새 400 응답으로 교체하지 않는다. `request_body_error` 증가는 요청 생성기와 앞단 프록시의 HTTP framing을 확인할 신호다. 이 지표는 upstream 장애율과 분리해 관측한다.

## 응답 중단과 확인된 upstream 5xx

Gateway가 upstream 5xx 헤더를 확인한 뒤 클라이언트가 응답 본문 수신을 중단해도 해당 요청은 차단기 실패 표본으로 남는다. ReverseProxy의 스트리밍 중단(panic 복구)과 마지막 buffered flush에 동일한 정책을 적용한다. 반대로 정상 upstream 응답의 downstream 쓰기 실패·클라이언트 취소는 계속 차단기 표본에서 제외한다.

차단기 표본과 요청 outcome은 서로 다른 질문에 답한다. 확인된 upstream 503 뒤 클라이언트가 연결을 끊으면 차단기 실패는 증가하면서 요청 outcome은 `stream_error`, `canceled` 또는 `timeout`일 수 있다. outcome은 전송이 끝난 방식을 나타내므로 확인된 5xx가 항상 `upstream_5xx` outcome으로 기록되는 것은 아니다. `/status`의 그룹별 `circuit_breaker.window_failures`와 `isolatex_breaker_state`, 전송 오류 outcome을 함께 확인한다.

응답 복사와 요청 본문 복사는 서로 다른 goroutine에서 끝날 수 있다. 전체 요청이 timeout이고 아직 본문 읽기가 끝나지 않았다면, 차단기 귀속을 정하기 전에 마지막 본문 읽기 결과를 최대 250ms 기다린다. 본문을 모두 받은 고정 길이 요청이나 확인된 backend 5xx는 기다리지 않는다. 이 대기는 기존 오류 응답 grace와 같은 크기의 별도 상한이며, 커스텀 reader가 결과를 늦게 반환해도 무기한 슬롯을 유지하지 않는다. 상한 안에 확인된 본문 timeout만 request_body_timeout으로 분류한다.

## HTTPS upstream의 TLS handshake 예산

HTTPS upstream의 연결 수립은 TCP 연결부터 TLS handshake 완료까지 원래 그룹 요청의 남은 기한과 취소를 따른다. ClientHello 이후 서버가 응답하지 않아도 요청이 끝난 뒤 미완료 TLS 소켓을 다음 요청의 연결 예열용으로 남기지 않는다. handshake 실패·취소 때 소켓을 닫아 해당 upstream의 연결 자리를 회수한다. TCP connect의 기존 3초 상한은 유지하며 TLS 단계는 그룹 요청의 남은 예산을 사용한다.

전체 요청 기한 초과는 기존 504와 upstream 실패 정책, 클라이언트 취소는 차단기 표본 제외 정책을 유지한다. 다른 그룹은 별도 연결 풀과 요청 예산을 사용한다. 이 정책은 전체 outbound FD 예약을 추가하는 기능이 아니며 DNS resolver의 내부 OS 작업까지 즉시 회수한다는 보장은 아니다.

TLS 인증서·호스트 이름 검증은 기본 TLS 정책대로 수행한다. handshake가 성공하면 취소 연결을 해제해 정상 keep-alive 재사용을 유지한다. Gateway 내부에서 TLS 설정을 사용하면 연결별 복제본에 대상 호스트 이름을 넣어 공유 설정을 바꾸지 않는다. 운영자는 504와 upstream TLS 연결 실패를 함께 확인하고 인증서 신뢰·만료·이름 불일치와 서버 handshake 정체를 구분해야 한다.

## 준비 상태 점검의 연결 수명

관리형 워커 점검과 외부 upstream의 정기 점검도 TCP/TLS 연결 수립에 해당 점검의 기한·취소를 적용한다. `readiness.timeout`은 HTTP Client가 적용한 최종 기한을 Transport 진입 시 보존해 DNS/TCP 연결 시도와 TLS handshake에 전달한다. 워커 시작 중에는 `startup_timeout`의 남은 기한도 함께 적용하고 Supervisor 종료 시 진행 중 연결 수립을 취소한다.

점검은 연결을 재사용하지 않는 기존 정책과 64KiB 응답 헤더 상한을 유지한다. TLS 서버가 ClientHello 뒤 멈췄을 때 실패할 때마다 새 미완료 소켓이 누적되지 않도록 한다. 점검 오류는 기존 준비 상태 연속 실패·성공 임계값과 `isolatex_health_check_failures_total`에 반영하며, 이 변경으로 앱을 임의로 재시작하지 않는다. Gateway와 점검은 동일한 내부 dial 구현을 사용하지만 Transport·연결 풀·요청 예산은 각각 유지한다.

전역 outbound FD 예약이나 OS DNS 내부 작업의 즉시 회수를 보장하는 기능은 아니다. 운영 시 준비 상태 실패 증가와 runtime FD·연결 수를 함께 확인하고, TLS 응답 정체와 인증서 검증 실패를 구분한다. 인증서·호스트 이름 검증은 기본 TLS 정책을 유지한다.

## HTTPS upstream 등록

정적 `upstreams`에 `https://upstream.example.com:443`처럼 HTTPS origin을 지정할 수 있다. 같은 런타임·그룹에서 HTTP와 HTTPS origin을 함께 등록할 수 있다. 기존과 같이 호스트와 명시적 포트 1–65535가 필요하며 URL 안의 자격 증명·경로·쿼리·fragment는 허용하지 않는다. [혼합 설정 예시](../examples/static-https.yaml)의 example 호스트를 실제 서버로 바꿔 사용한다.

Gateway 요청과 정적 readiness 점검 모두 TLS 서버 인증서·호스트 이름을 검증한다. 신뢰 루트는 런타임의 기본 OS/Go 인증서 저장소를 사용하며 자체 서명 또는 사설 CA 서버는 운영 환경에서 해당 CA를 신뢰하도록 구성해야 한다. YAML에 인증서 검증을 끄는 옵션은 제공하지 않는다. 인증서 검증 실패는 Gateway에서 502, readiness에서 실패 점검으로 처리한다. TLS 서버 응답 정체는 앞서 설명한 요청·점검 기한과 취소에 따라 회수한다.

`-check`는 HTTPS origin의 구문과 설정 예산을 검사하며 DNS·서버 연결·인증서 신뢰를 실제로 검사하지 않는다. 예시에는 임의 API가 제공하지 않을 수 있는 readiness 경로를 넣지 않았다. 실제 상태 점검 URL을 알고 있을 때 readiness를 설정한다. 생략하면 해당 정적 upstream이 준비됐다고 가정하는 기존 정책을 따른다.

이 기능은 upstream으로 향하는 연결의 TLS 지원이다. 공용·관리 리스너가 HTTPS를 직접 수신하는 기능은 추가하지 않으며 클라이언트 측 TLS 종단은 계속 외부 프록시에서 구성한다.

## 준비 상태 경로와 origin의 마지막 슬래시

정적 upstream origin은 `http://host:port`와 `http://host:port/`를 모두 허용하며 HTTPS도 같다. readiness는 origin의 빈 경로 또는 루트 경로를 설정된 `readiness.path`로 교체한다. 따라서 두 표기 모두 `/readyz`를 점검하며 `//readyz`를 만들지 않는다. Pool에 저장된 upstream URL은 바꾸지 않는다.

점검 경로 자체의 마지막 슬래시는 유지한다. `/readyz/`를 설정하면 그 경로를 요청한다. 서버가 다른 경로로 리다이렉트하더라도 따라가지 않는 기존 정책은 유지하므로 실제 서버의 점검 경로를 정확히 지정해야 한다. 관리형 워커 점검에도 같은 URL 생성 함수를 사용한다.

## Upstream 정보성 응답(1xx) 한도

Gateway는 upstream의 최종 응답 전 정보성 응답(예: 100 Continue, 103 Early Hints)을 요청당 최대 8개 전달한다. 전달할 정보성 응답의 누적 헤더 예산은 64KiB다. 계산은 응답마다 상태 줄·빈 줄 용도로 32바이트를 차감하고, 파싱된 각 헤더 값마다 이름·값 바이트 수와 구분자 4바이트를 차감한다. 원본 wire 바이트 수와 같은 계측은 아니다. 개별 응답의 기존 64KiB Transport 헤더 한도와 최종 응답 헤더 한도도 유지한다.

정보성 응답을 사용자에게 전달하는 Go Transport 경로는 각 1xx 뒤에 헤더 한도를 다시 시작한다. 따라서 개별 헤더 크기 제한만으로 응답 개수와 누적 처리를 제한할 수 없다. IsolateX는 초과한 정보성 응답을 클라이언트로 전달하기 전에 upstream 요청을 중단하고 최종 HTTP 502 / `upstream_informational_limit`을 반환한다. 이 요청은 차단기 실패로 집계하고 실행 슬롯과 워커 선택을 반환한다. 이미 전달한 정상 1xx 응답은 취소되지 않는다. 요청 기한 초과·클라이언트 취소가 함께 발생하면 기존 오류 우선순위를 따른다.

운영 시 `isolatex_requests_total{outcome="upstream_informational_limit"}` 증가를 확인하고 upstream의 Early Hints·정보성 응답 반복과 헤더 크기를 조사한다. 제한 안의 정보성 응답은 기존처럼 전달하며 최종 응답 결과로 차단기에 반영한다. 이 한도는 요청 전체 기한이나 응답 본문 스트리밍 기한을 대체하지 않는다.


## Early responses while an upload remains open

Admitted proxy requests enable HTTP/1 full duplex through ResponseController so
response-header flushing does not make the server drain the original request
body outside the gateway's upload tracker. This permits an upstream response to
begin before the client finishes uploading; it does not enable the unsupported
WebSocket, gRPC or SSE protocols. The same body limit and request/socket deadlines
still apply.

Closing an upload body can itself drain pending input and return a socket read
timeout. That is client timeout evidence just like a timeout returned by Read.
The gateway observes both before completing circuit attribution within the
existing 250ms grace. A confirmed request-body timeout is ignored by the breaker;
a completed upload followed by a backend stall/reset still counts as failure,
and an already observed upstream 5xx remains failure evidence. Merely closing a
body, or getting ErrBodyReadAfterClose, is not sufficient client-timeout evidence.
