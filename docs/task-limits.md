# 워커 태스크·프로세스 격벽

요청 수와 메모리 한도만으로는 자손 생성 폭주가 호스트의 PID·스레드를 소진하는 일을 막지 못한다. 관리형 `process.resources`에서 **워커 하나와 자손 전체**의 생성 한도를 선택할 수 있다. 외부 upstream에는 적용하지 않는다. 그룹의 워커 수가 2라면 각각 독립된 예산을 갖는다.

Linux:

```yaml
resources:
  tasks_max: 128
  cgroup_parent: /sys/fs/cgroup/system.slice/isolatex.service
  # memory_max_bytes: 268435456  # 필요하면 함께 설정
```

Windows:

```yaml
resources:
  processes_max: 8
  # memory_max_bytes: 268435456
```

| 설정 | 지원 환경 | 집계 범위 | 한도 도달 시 |
| --- | --- | --- | --- |
| `tasks_max` | Linux cgroup v2 | 워커 및 자손의 커널 태스크, 스레드 포함 | 추가 fork/clone 거부 (`EAGAIN`) |
| `processes_max` | Windows Job Object | 워커 및 자손의 활성 프로세스, 스레드 제외 | 추가 프로세스 생성·등록 거부 |

두 설정은 다른 단위이므로 플랫폼에 맞지 않는 양수 옵션을 지정하면 설정 오류다. Windows 값은 DWORD 범위 내여야 한다. 한도에는 워커 자신도 포함한다. 스레드를 만드는 Go·JVM 등의 앱에는 시작 및 정상 동작에 필요한 여유를 준다. Windows의 `processes_max`는 단일 프로세스의 스레드 폭주를 제한하지 않는다. [Linux PID 컨트롤러](https://docs.kernel.org/admin-guide/cgroup-v2.html#pid), [Windows 활성 프로세스 한도](https://learn.microsoft.com/en-us/windows/win32/api/winnt/ns-winnt-jobobject_basic_limit_information#members).

각 자원 한도는 0 또는 생략 시 비활성이다. `resources`를 지정했다면 적어도 하나의 한도는 양수여야 한다. 메모리 한도 없이 생성 수만 제한할 수 있고, 메모리·생성 수를 함께 제한할 수도 있다. 음수·잘못된 필드·미지원 OS는 거부한다. 커널 기능·권한·컨트롤러가 없으면 해당 워커를 실행하지 않는다. Linux에서는 요청한 컨트롤러만 필요하며, 태스크 전용 설정은 `memory.max`나 swap 정책을 변경하지 않는다.

Linux는 위임된 부모의 `cgroup.subtree_control`에 `+pids`가 활성화되어 있어야 한다. 메모리와 병용하면 `+memory +pids`가 필요하다. 런타임과 워커가 같은 위임 범위에 속해야 하는 조건 및 정리 계약은 [메모리 격벽 문서](memory-limits.md#linux-위임과-systemd)와 같다. [systemd 예제](../examples/memory/isolatex.service)는 두 컨트롤러를 위임한다. 앱 코드 실행 전에 cgroup FD 또는 정지 상태의 Job 등록으로 한도를 적용하며, 런타임 소유자 Job에는 워커 생성 한도를 걸지 않는다.

생성 거부 자체는 워커 강제 종료 사유가 아니다. 오류를 처리한 앱은 같은 PID로 계속 서비스할 수 있다. 앱이 종료되면 기존 재시작·백오프 정책을 따른다. Linux에서 스레드 생성 실패를 치명 오류로 처리하는 언어 런타임은 종료할 수 있다. 종료 시 자손 정리 실패는 해당 슬롯을 `failed`로 멈추며 새 세대의 예산을 겹치지 않는다.

`/status`의 `worker_tasks_max`, `worker_processes_max`와 Prometheus의 `isolatex_worker_tasks_limit{group=...}`, `isolatex_worker_processes_limit{group=...}`는 설정된 워커당 상한이다. 0은 미설정이며 현재 사용량이나 초과 횟수가 아니다. 커널·상위 Job/cgroup의 더 작은 상한은 여전히 적용된다. CPU 시간, 파일 디스크립터, DB 연결을 제한하지 않으며 같은 권한의 악성 앱을 가두는 샌드박스가 아니다.

검증은 최대 48개 자손만 만드는 유한한 3세대 트리로 수행한다. 무제한 대조는 48개 모두 생성하며, 제한 시 자손의 생성 거부·정리 후 재요청·보호 경로의 연속 HTTP 성공과 PID 유지를 확인한다. Windows의 오류를 처리한 워커는 같은 PID를 유지한다. Linux Go 런타임이 스레드 부족으로 종료하면 해당 워커의 복구를 확인하며, HTTP 실패만으로 통과시키지 않고 커널 pids 거부 기록을 요구한다. 메모리 한도와의 병용도 검사한다. Linux는 별도로 `LockOSThread`를 사용해 실제 스레드 생성을 한도에 도달시킨 후 `pids.events`의 커널 거부 기록을 확인한다. 컨트롤러 미위임 시 앱이 실행되지 않고 새 cgroup도 남지 않아야 한다.

```sh
sudo bash scripts/check-memory-linux.sh "$(id -un)" "$(command -v go)"
```

기존 이름의 위 도구는 메모리와 태스크 검사를 함께 실행한다. Go 테스트와 앱은 일반 사용자로 실행하며 자신의 임시 위임 범위만 구성·정리한다. Linux CI에서는 위 검사를 생략하지 않는다.
