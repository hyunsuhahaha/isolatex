# 워커 CPU 격벽

CPU를 계속 소모하는 관리형 워커와 자손 전체에 선택적 상한을 적용한다. 요청 동시 실행 수·메모리·생성 수 제한과 독립적이다. CPU 제한만 지정하거나 다른 한도와 병용할 수 있다. 외부 upstream의 CPU는 제어하지 않는다.

Linux:

```yaml
resources:
  cpu_millicores: 500  # 워커와 자손의 합: 코어 하나의 0.5배
  cgroup_parent: /sys/fs/cgroup/system.slice/isolatex.service
  # memory_max_bytes: 268435456
  # tasks_max: 128
```

Windows:

```yaml
resources:
  cpu_percent: 10  # Windows Job CPU rate 기준 10%
  # memory_max_bytes: 268435456
  # processes_max: 8
```

| 설정 | 지원 OS | 단위와 처리 |
| --- | --- | --- |
| `cpu_millicores` | Linux | 1000m = 논리 코어 하나의 대역폭. 100ms 고정 주기의 `cpu.max` quota로 설정 |
| `cpu_percent` | Windows | 1..100 정수 비율. 워커 Job의 CPU rate에 ENABLE·HARD_CAP 적용 |

Linux는 250m를 `cpu.max = 25000 100000`으로 설정한다. 2000m는 두 코어 상당의 CPU 시간이다. 활성 값의 최소는 10m(100ms 주기에서 quota 1ms)이며 quota 계산이 int64로 표현되어야 한다. 실제 커널·상위 계층의 추가 제약은 워커 시작 때 확인한다. CPU controller가 적용되는 일반 fair-class 스케줄러를 대상으로 하며 RT·DEADLINE 작업을 제한하는 계약은 아니다. [Linux CPU 인터페이스](https://docs.kernel.org/admin-guide/cgroup-v2.html#cpu-interface-files), [대역폭 주기와 스케줄러 동작](https://docs.kernel.org/scheduler/sched-bwc.html).

Windows는 코어 수 대신 Job CPU rate를 사용한다. 상위 Job에 CPU rate 제한이 없으면 시스템 CPU 기준이며, 제한이 있는 상위 Job에 중첩되면 바로 위 Job의 CPU 할당에 대한 비율이 된다. 따라서 Linux의 500m와 Windows의 50%는 같은 설정이 아니다. DFSS가 적용되는 Remote Desktop Services 환경 등 API가 지원되지 않는 조건에서는 워커 시작이 실패한다. [Windows CPU rate 계약](https://learn.microsoft.com/en-us/windows/win32/api/winnt/ns-winnt-jobobject_cpu_rate_control_information)을 따른다.

메모리·생성 수·CPU의 숫자는 YAML 정수로 지정한다. 소수값(`0.9`, `10.0` 포함)·숫자 문자열·불리언·비유한 수는 거부한다. 소수값이 정수 0으로 잘려 제한이 꺼지는 일을 막는다. 정수 anchor·merge는 지원하며 잘못된 소수값을 anchor·merge로 넣어도 거부한다. 0·생략은 해당 한도를 비활성화하며 명시적인 YAML null은 설정 오류다. `resources`에는 적어도 하나의 양수 한도가 필요하다. 플랫폼에 맞지 않는 양수 옵션·음수·Windows 100% 초과·Linux 최소 미만·quota 산술 오버플로는 설정 오류다. macOS 등은 현재 CPU 옵션을 지원하지 않는다.

## 격리와 운영 조건

각 워커 트리가 독립된 한도를 갖는다. 워커가 2개이면 각 트리에 지정값을 적용하며 그룹 전체의 합산 예산은 아니다. Linux는 cgroup FD로 생성하기 전에 quota를 설정하고, Windows는 정지 워커를 CPU 제한 Job에 등록한 뒤 실행한다. 컨트롤러·권한·Job 설정 실패를 제한 없는 실행으로 우회하지 않는다. 런타임 소유자 Job에는 워커 CPU 제한을 걸지 않는다.

상한을 소진하면 스케줄링 주기에 따라 실행이 지연된다. CPU 초과 자체로 앱을 종료하지 않지만, 너무 낮은 값은 시작·준비 상태·요청의 기한 초과를 유발할 수 있다. 앱의 시작 비용과 정상 처리량에 맞춰 한도와 타임아웃을 조정한다. 주기·스케줄러·CPU 회계 차이 때문에 순간 점유율의 정확한 벽이나 일정한 응답 시간을 뜻하지 않는다.

Linux는 위임 부모의 `cgroup.subtree_control`에 `+cpu`가 필요하다. 메모리·태스크 제한을 함께 쓰면 각각 `+memory`, `+pids`도 필요하다. [service 예제](../examples/memory/isolatex.service)는 세 컨트롤러를 위임하고 `DelegateSubgroup=manager`로 런타임을 별도 자식에 배치한다. systemd 254 이상 예제이며 운영 계정·경로·service 이름을 실제 환경에 맞춘다. 런타임 자체는 systemd 전용이 아니며 동일한 권한과 계층을 갖춘 cgroup v2 위임도 사용할 수 있다.

CPU 상한은 CPU 예약이나 보호 그룹의 처리량 보장이 아니다. 모든 워커·호스트 서비스의 전체 예산, 상위 Job/cgroup의 제한, 공유 게이트웨이·DB·I/O 경합은 별도로 고려한다. 같은 사용자 권한의 악성 앱이 정책을 바꾸는 것을 막는 샌드박스도 아니다. 종료와 정리 실패 계약은 [메모리 격벽](memory-limits.md)과 같다.

## 관측과 재현

`/status`의 `worker_cpu_millicores`, `worker_cpu_percent`와 Prometheus의 `isolatex_worker_cpu_millicores_limit{group=...}`, `isolatex_worker_cpu_percent_limit{group=...}`는 설정된 워커당 상한이다. 0은 미설정이며 실제 CPU 사용률·throttling 게이지가 아니다.

검사는 부모와 실제 자손이 각각 유한한 시간 동안 CPU를 소모하는 작업을 사용한다. CPU 무제한 대조·CPU 단독 제한·메모리 및 생성 수 병용을 검사한다. Linux는 부모·자손의 동일 cgroup/quota, `cpu.stat`의 실제 throttling 증가와 합산 사용 시간을 확인한다. Windows는 두 프로세스의 실제 Job hard cap, 계산 수행량·QueryProcessCycleTime 증가, Job의 누적 프로세스 수와 보고된 합산 CPU 시간의 보조 상한을 확인한다. 낮은 CPU 비율에서는 기존 시간 카운터가 증가하지 않을 수 있으므로 작업 수행 여부를 그 카운터 하나에 의존하지 않는다. [프로세스 사이클 API](https://learn.microsoft.com/en-us/windows/win32/api/realtimeapiset/nf-realtimeapiset-queryprocesscycletime)의 사이클 수를 시간이나 응답 SLO로 변환하지 않는다. 보호 경로의 연속 HTTP 성공, 워커 PID·재시작 0, 상태·지표도 검사한다. 매우 낮은 CPU 한도로 격리 워커의 준비 상태가 일시적으로 실패하면 부하 종료 후 같은 PID로 회복해야 한다. 이 검사는 성능 향상율이나 운영 SLO를 입증하는 벤치마크가 아니다.

```sh
sudo bash scripts/check-cpu-linux.sh "$(id -un)" "$(command -v go)"
```

Linux CI의 위 검사는 systemd 254 이상에서 자신의 임시 service만 만들고, 지정한 일반 사용자로 실제 커널 검사를 race 2회 실행한다. 전역 컨트롤러 파일을 직접 변경하지 않으며 종료 시 정확히 해당 unit을 중단·회수한다. CPU controller가 없는 별도 하위 계층에서는 앱 실행과 cgroup 누출이 없어야 한다. 위임 환경변수가 없는 일반 Linux 테스트는 실제 CPU 검사를 skip하므로 CI의 별도 위임 검사를 생략하지 않는다. Windows는 일반 전체 테스트에서 실제 Job 검사를 실행한다.

## Windows 반복 진단 원자료

저장소 checkout과 Go SDK, PowerShell 7.2 이상이 있는 네이티브 Windows에서 실행한다.

```powershell
pwsh -NoProfile -File scripts/check-cpu-windows.ps1 -Count 3
# Go가 PATH에 없을 때:
pwsh -NoProfile -File scripts/check-cpu-windows.ps1 -GoBinary 'C:\Go\bin\go.exe' -Count 3
```

Count는 1–20이며 회마다 무제한 대조·CPU 단독·메모리 및 프로세스 수 병용의 실제 부모/자손 부하를 실행한다. 각 부하는 기존 검사와 같이 유한한 1.5초 계산 구간을 사용한다. 이 명령은 설정 readback만 확인하는 검사가 아니며 기존 CPU 사용량 상한·실제 계산 진행·보호 경로·PID 유지 검사를 그대로 실행한다. 실패를 재시도해서 숨기거나 합격선을 변경하지 않는다.

기본 `.tools/cpu-check/<UTC 시각>-<고유 ID>/`에 새 실행 디렉터리를 만들고 경로를 출력한다. `-OutputDirectory`로 부모 위치를 바꿀 수 있다. 기존 기록을 덮어쓰거나 자동 삭제하지 않는다.

- `summary.json`: Go 버전·빌드 환경, Windows build, CPU 수, 소스 커밋·dirty 여부, 테스트 인수, 모드별 Before/After/Child/ChildAfter/Elapsed 원자료와 결과를 담는다. `UsageUsec`는 Job 누적 CPU 마이크로초, `Cycles`는 해당 프로세스의 원시 cycle 계수, `Elapsed`는 나노초다. 작업량과 cycle 수를 시간·SLO로 변환하지 않는다.
- `events.jsonl`: `go test -json`의 원본 이벤트이며 실패한 subtest와 원자료를 함께 보존한다.
- `stderr.log`: Go 명령의 별도 stderr다. 컴파일 실패처럼 테스트 실행 전의 오류도 남긴다.

소스 fingerprint는 Go 파일·go.mod/go.sum·진단 스크립트의 경로와 SHA-256으로 계산한다. 실행 전후의 fingerprint가 다르면 성공으로 보고하지 않는다. 이는 해당 파일들의 실행 시점 비교이며 OS·외부 도구의 보안 증명은 아니다. dirty=true인 기록은 커밋 SHA만으로 같은 소스라고 간주하지 않는다.

Go 종료 코드가 0이어도 모든 모드의 반복별 pass 이벤트와 유효한 부모·자손 원자료가 없으면 진단은 실패한다. 이때도 보고서를 남기고 명령은 0이 아닌 코드로 끝난다. 원자료 누락·손상과 빌드 실패가 성공으로 기록되지 않는 음성 대조를 Windows CI에서 검사한다.

2026-10-30에는 같은 로컬 Windows에서 CPU 상한 실패와 이후 변경 없는 코드의 성공을 모두 관측했다. 현재 원인은 미확정이며 이후 성공이 과거 실패의 해결을 뜻하지 않는다. [검증 기록](verification.md)을 참고하고 실패·성공 양쪽의 원자료와 환경을 함께 비교한다. Microsoft의 [CPU rate 계약](https://learn.microsoft.com/en-us/windows/win32/api/winnt/ns-winnt-jobobject_cpu_rate_control_information)과 [즉시 Job의 계수 조회](https://learn.microsoft.com/windows/win32/api/jobapi2/nf-jobapi2-queryinformationjobobject)를 구분해 읽으며, 설정 값 조회만으로 실제 집행을 단정하지 않는다.


## Windows 실제 작업량 대조

Job의 기존 CPU 시간 계수가 증가하지 않는 환경에서도 단순한 설정 조회와 작업
진행만으로 집행 성공을 판정하지 않는다. 전체 CPU 검사는 같은 실행의 무제한
trial을 기준으로 부모·자손의 합산 계산 batch 수를 실제 경과 시간으로 나눈 뒤
CPU 단독·병용 trial과 비교한다. 시간 계수와 독립적인 보조 대조다.

고정 fixture는 프로세스마다 CPU goroutine 4개를 사용한다. 논리 CPU 1–64개에서는
1% 설정의 트리 전체 허용량이 최대 0.64 CPU이므로, 제한 trial의 계산 속도가
무제한 대조의 75% 이하여야 한다. 이는 큰 미집행을 검출하는 느슨한 smoke 기준이며
정확한 1% 사용률·코어 시간·운영 성능 보장이 아니다. 기존 Job 설정·시간 상한·각
프로세스의 실제 진행·보호 경로 검사도 유지한다. 실제 무제한 trial을 제한 결과로
평가하면 이 작업량 검사를 통과해서는 안 된다는 음성 대조도 실행한다.

과도한 호스트 경쟁 때문에 무제한 기준 trial부터 굶으면 작업량 감소를 입증하지
못할 수 있다. 이 경우 집행 상태와 호스트 부하를 조사하고 원자료를 보존한다.
실패를 자동 재시도하거나 CPU 시간 계수가 0이라는 이유로 성공으로 바꾸지 않는다.

64개 초과 논리 CPU에서는 1%가 이 고정 부하를 충분히 제한한다고 단정하지 않으므로
작업량 대조를 집행 증거로 사용하지 않는다. 기존 native 검사는 계속 실행하며,
`cpu_work_reduction` 로그에 `checked=false`를 명시한다. 한 limited subtest만 선택하면
무제한 기준이 없어 작업량 대조를 수행하지 않았다고 기록한다. 반복 진단 스크립트는
항상 세 모드를 모두 실행한다. `events.jsonl`에서 모드별 checked·ratio·ceiling을
확인하고 summary의 원본 작업량·경과 시간과 함께 해석한다.
