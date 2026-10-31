# Linux·macOS 관리형 프로세스 그룹

각 관리형 워커는 exec 전에 새 프로세스 그룹의 리더가 된다. 일반적으로 워커가 생성하는 자손은 같은 그룹을 상속한다. 종료할 때 해당 그룹에만 신호를 보내므로 다른 워커·그룹은 유지된다. macOS는 이전 개발 커밋의 직접 자식 종료에서 변경됐다.

## 종료 순서

정상 런타임 종료에서는 HTTP 요청을 drain한 뒤 프로세스 그룹 전체에 SIGTERM을 보낸다. 직접 자식이 1초 안에 종료하지 않으면 그룹에 SIGKILL을 보내고 직접 자식을 수거한다. 부모 워커가 먼저 종료됐어도 남은 그룹 구성원에게 SIGKILL을 보내 재시작 전에 정리한다.

각 워커 세대가 별도 제어 객체를 가지며 정리 완료 후에는 추가 신호를 보내지 않는다. 같은 자리에 새 워커가 생겼다고 이전 세대의 정리가 다시 신호를 보내지 않는다.

신호 전송과 자손의 커널 상태 전환은 비동기다. 직접 자식은 Wait로 수거하고, 검사는 자손의 실제 실행 중단을 별도의 유한 대기로 확인한다.

앱은 foreground로 실행해야 한다. 스스로 daemonize하거나 setsid/setpgid로 그룹을 벗어난 프로세스는 프로세스 그룹 경계로 관리되지 않는다. Linux에서 선택적 [cgroup 메모리 예산](memory-limits.md)을 사용하면 cgroup 내부 자손도 강제 종료 대상이다. 런타임 자체가 SIGKILL로 종료되거나 호스트가 실패하면 정리 코드 실행을 보장하지 않는다. 선택적 [CPU quota](cpu-limits.md)는 지원하며 DB quota나 악성 코드 샌드박스는 제공하지 않는다.

## 검증

```sh
go test ./internal/supervisor -run 'TestProcessGroupContains|TestManagedGroupReaps' -count=10 -timeout=90s
go test -race ./... -count=1 -timeout=90s
```

검사는 실제 셸이 sleep 자손을 생성하게 한다. 직접 부모만 종료하는 음성 대조에서는 자손이 살아 있어야 하며 검사 종료 시 해당 자손을 제거한다. 관리형 그룹에서는 부모·자손의 그룹 ID 일치, 종료 후 자손의 실행 중단, 별도로 띄운 워커의 생존을 확인한다. 직접 자식은 Wait로 수거한다. 종료된 자손이 init/launchd에 수거되기 전 잠시 zombie로 남는 상태는 실행 중단으로 판정한다.

Linux는 `/proc`로 자손 상태를 확인하고 macOS는 기본 `ps`를 사용한다. 로컬 Linux race 실행, Darwin용 크로스 컴파일, 네이티브 macOS CI 실행은 서로 다른 검증이며 기록에서 구분한다. Linux·Windows·macOS 이외의 OS는 현재 직접 자식 종료 범위다.

실제 Supervisor 검사에서는 SIGTERM을 무시하고 stdout/stderr를 상속하는 자손을 가진 워커를 HTTP로 크래시시킨다. 이전 자손 제거 후 대체 워커·자손을 시작하고, 다른 그룹의 기존 PID가 HTTP 응답을 계속 주는지 확인한다. 최종 Stop에서도 SIGTERM을 무시하는 대체 자손이 제거돼야 한다.

## 시작 실패와 자원 정리

관리형 워커가 준비되기 전에도 Linux cgroup 또는 Windows Job이 먼저 생성될 수 있다. 자원 설정·프로세스 생성·Windows Job 연결 단계의 시작 오류와 정리 오류를 함께 보존한다. 정리가 확인되지 않으면 `worker_resource_cleanup_failed`를 기록하고 해당 슬롯을 `failed`로 멈춘다. 이 경우 backoff와 새 워커 생성으로 넘어가지 않는다. 정리가 성공한 시작 실패는 기존 재시작 예산 안에서 재시도한다.

다른 슬롯·그룹의 워커는 계속 동작한다. `failed` 슬롯은 자동으로 다시 열리지 않으므로 `/status`와 오류 로그에서 원인을 확인하고 남은 OS 자원을 운영 환경에서 조사한 뒤 런타임을 다시 시작한다. 이 정책은 정리 실패 이후 추가 자원 예산이 쌓이는 것을 막으며, 이미 정리에 실패한 자원이 모두 회수됐다는 뜻은 아니다.

종료 요청 후 직접 자식의 종료가 확인되지 않을 때의 대기 상한과 terminal 상태는 [종료 대기 정책](worker-exit.md)을 따른다.


## Linux 기본 프로세스 그룹 정리 확인

Linux에서 `resources`를 생략한 워커도 세대의 `close` 단계에서 그룹에 SIGKILL을
요청하고 실행 중인 멤버가 남는지 확인한다. 이전의 별도 signal 호출 여부에 의존하지
않는다. 그룹이 사라지면 즉시 완료하며, 남아 있으면 `/proc`에서 해당 그룹 멤버의
상태를 조회한다. zombie/dead 멤버만 남은 상태는 실행 중단으로 인정한다. 다만
zombie인 리더에게 다른 스레드가 남아 있으면 완료로 보지 않는다. 직접 자식의 Wait는
별도로 유지한다. 이는 고아 zombie를 수거했다는 뜻이 아니다.

확인 polling은 최대 1초이며 디렉터리는 128개씩 읽고 전체 65,536 entry, stat 레코드는
4KiB로 제한한다. Getpgid로 다른 그룹을 먼저 제외하므로 다른 그룹의 stat 권한이나
명령 내용은 읽지 않는다. 멤버를 확인할 수 없거나 읽기·형식·예산 오류가 생기면
같은 1초 예산 안에서 다시 관측하고, 끝내 확인되지 않으면 정리 실패로 반환한다. 결과는 세대별로 보존해 이후 close가 성공으로 바꾸지 않는다.
이때 기존 `worker_resource_cleanup_failed`와 terminal `failed` 정책에 따라 대체 워커를
추가하지 않는다. 다른 그룹은 자체 수명과 요청 예산으로 계속 동작한다.

프로세스 그룹과 같은 PID namespace를 보여 주는 procfs와 워커 멤버의 stat 읽기
권한이 필요하다. 숨겨진 멤버·자격 증명/namespace 변경을 안전하게 탐지하는 샌드박스가
아니며, 그룹을 이탈한 자손은 기존 프로세스 그룹 계약 밖이다. 커널 호출 자체가 막힌
경우 userspace polling 기한이 그 호출을 중단하지는 못한다. 숨김 정책·대규모 process
inventory·커널 대기 때문에 확인이 실패하면 로그의 원래 PID와 OS 상태를 조사한다.

Linux cgroup 자원을 설정한 워커는 기존 cgroup.kill과 디렉터리 회수 검사를 유지한다.
macOS의 프로세스 그룹에는 이번 Linux 전용 procfs 확인을 적용하지 않는다.
[`proc_pid_stat` 필드](https://www.man7.org/linux/man-pages/man5/proc_pid_stat.5.html)와
[커널 procfs 문서](https://www.kernel.org/doc/html/latest/filesystems/proc.html)를 따른다.
