# 워커 메모리 격벽

관리형 워커 하나와 그 자손에 선택적 메모리 예산을 적용한다. 경로별 요청 예산과 별개다. `resources`를 생략하면 기존 실행 방식이며 메모리 한도를 설정하지 않는다. 외부 `upstreams`에는 적용하지 않는다.

```yaml
process:
  command: ["./bin/demo-app"]
  workers: 2
  port_start: 9100
  resources:
    memory_max_bytes: 268435456  # 워커 트리 하나당 256 MiB
    # Linux에서만 필요. Windows에서는 이 줄을 생략한다.
    cgroup_parent: /sys/fs/cgroup/system.slice/isolatex.service
```

양의 바이트 수를 지정한다. 메모리 값 0·생략은 메모리 한도를 비활성화하며, `resources`에는 적어도 하나의 양수 한도가 필요하다. `resources: {}`는 오류다. [태스크·프로세스 한도](task-limits.md)와 독립적으로 또는 함께 사용할 수 있다. 워커 2개에 256MiB를 지정하면 그룹 전체 256MiB가 아니라 **각 트리 256MiB**다. 상한은 RAM 예약이 아니므로 런타임과 다른 서비스의 여유를 포함해 호스트 전체 예산을 잡아야 한다. 너무 작은 한도는 앱의 로더·런타임 시작부터 실패시킬 수 있다.

## OS별 처리

| 환경 | 강제 경계 | 초과 시 동작 |
| --- | --- | --- |
| Windows | 워커 Job의 `JOB_OBJECT_LIMIT_JOB_MEMORY` | 트리 전체의 commit 추가 할당 거부. 앱이 오류를 처리하거나 종료할 수 있음 |
| Linux | 워커 cgroup v2의 `memory.max` | 회수 후에도 부족하면 해당 cgroup의 OOM 처리. `memory.oom.group=1`, swap 한도 0 적용 |
| macOS·그 외 | 현재 미지원 | 옵션을 사용하면 설정 오류 |

두 OS의 수치는 동일한 RSS 계측값이 아니다. Linux는 cgroup 회계, Windows는 committed memory를 사용한다. Linux 커널의 일시적인 한도 초과·회수 동작도 있으므로 바이트 단위의 순간 RSS 벽으로 해석하지 않는다. [Linux 메모리 컨트롤러](https://docs.kernel.org/admin-guide/cgroup-v2.html), [Windows Job 메모리 계약](https://learn.microsoft.com/en-us/windows/win32/api/winnt/ns-winnt-jobobject_extended_limit_information)을 따른다.

Windows는 한도를 설정한 Job에 정지 워커를 등록한 뒤 실행한다. Linux는 미리 제한을 설정한 cgroup FD를 `clone3`에 전달한다. 실행 중인 PID를 나중에 옮기는 틈을 두지 않는다. 커널·컨트롤러·권한·Job 등록이 요구 조건을 충족하지 못하면 해당 워커는 실행되지 않으며 기존 재시작 예산에 실패가 반영된다. 제한 없는 실행으로 우회하지 않는다. `-check`는 스키마·플랫폼을 검사하며 실제 커널 권한과 앱의 필요 메모리는 워커 시작 시 확인한다.

앱이 종료되면 기존 워커 재시작·백오프·예산을 따른다. 할당 실패를 처리하고 계속 실행하는 Windows 앱을 자동으로 종료하지는 않는다. 현재 재시작 카운터는 메모리 원인만 별도로 구분하지 않는다.

## Linux 위임과 systemd

cgroup v2의 memory controller와 `memory.swap.max`, `memory.oom.group`, `cgroup.kill`, `clone3`의 cgroup FD가 필요하다. 부모 경로는 관리자가 위임한 디렉터리이며 memory controller를 자식에 활성화해 두어야 한다. 런타임도 같은 위임 범위의 별도 자식 cgroup에서 실행해야 생성 시 이동 권한을 만족한다. 일반 임시 폴더나 다른 위임 범위의 경로는 사용할 수 없다. IsolateX는 호스트 전역 컨트롤러 정책을 바꾸지 않는다.

[예제 service](../examples/memory/isolatex.service)는 systemd 254 이상에서 `Delegate=cpu memory pids`와 `DelegateSubgroup=manager`로 이를 구성한다. 운영 계정·설치/설정 경로·service 이름을 실제 환경에 맞춰 바꾼다. service 이름이나 slice를 바꾸면 YAML의 `cgroup_parent`와 `ExecStartPre` 경로도 함께 바꿔야 한다. [systemd 위임 계약](https://www.freedesktop.org/software/systemd/man/latest/systemd.resource-control.html#DelegateSubgroup=)을 참고한다.

예제의 `OOMPolicy=continue`는 워커 OOM을 서비스 전체 중단으로 확대하지 않기 위한 설정이다. `stop`·`kill` 정책이나 상위 서비스 매니저 정책이 전체 런타임을 종료하면 보호 그룹도 영향을 받는다. [systemd OOM 정책](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html#OOMPolicy=)과 호스트 정책을 함께 검토한다.

정상·강제 워커 종료는 Linux cgroup의 자손까지 종료하고 해당 세대의 빈 디렉터리를 제거한다. 정리 실패는 `worker_resource_cleanup_failed`로 기록하고 해당 슬롯을 `failed`로 멈춘다. 이전 자원의 종료를 확인하지 못한 상태에서 새 세대의 예산을 겹쳐 만들지 않는다. 이 정책은 프로세스 시작 이전의 cgroup 설정 실패와 실행 파일 시작 실패에도 적용한다. 생성한 cgroup의 제거·정리가 확인되지 않으면 일반 시작 오류와 구분해 재시작 대기를 거치지 않고 해당 슬롯을 failed로 멈춘다. 정리가 성공한 일반 시작 오류는 기존 재시작 예산 안에서 재시도한다. 런타임 SIGKILL 이후 cgroup 정리는 systemd의 전체 제어 그룹 종료 정책 등 운영 환경이 담당한다. 앱이 임의로 cgroup 구조를 만들거나 같은 권한으로 제한을 바꾸는 행위를 막는 보안 샌드박스는 아니다.

## 관측과 검증

`/status`의 그룹별 `worker_memory_max_bytes`와 `isolatex_worker_memory_limit_bytes{group=...}`는 **설정된 워커당 한도**다. 생략 시 0이며 실제 사용량 게이지가 아니다. 한도가 있는 워커의 PID·준비 상태·재시작과 다른 그룹의 응답을 함께 확인한다.

Windows의 전체 테스트에 실제 Job 제한 검사가 포함된다. Linux에서 위임 권한이 없는 일반 테스트는 실제 할당 검사를 skip하며, CI는 별도의 위임 검사로 이를 반드시 실행한다.

```sh
sudo bash scripts/check-memory-linux.sh "$(id -un)" "$(command -v go)"
```

이 도구는 고유한 임시 cgroup만 만들고, Go 테스트와 앱은 지정한 일반 계정으로 실행한다. 호스트 전역 컨트롤러를 변경하지 않는다. 마지막에는 자신이 만든 빈 cgroup 디렉터리만 제거하며 남은 자손/디렉터리가 있으면 실패한다.

제한 없는 대조에서는 부모 96MiB 또는 부모 80MiB+자손 176MiB 할당이 성공한다. 제한 검사에서는 각각 트리 한도 64MiB·256MiB를 적용해 실패·재시작과 보호 그룹의 연속 HTTP 응답·PID 유지를 확인한다. 이 기능은 공유 DB 상한이나 호스트 전체 OOM 방지를 제공하지 않는다. [CPU 상한](cpu-limits.md)은 별도로 선택할 수 있다.
