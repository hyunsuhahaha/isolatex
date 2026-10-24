# Windows 관리형 프로세스 수명

관리형 워커 하나가 자식을 생성했을 때 워커의 종료·재시작이 그 자식을 남기지 않도록 Job Objects를 사용한다. 다른 워커 세대와 다른 그룹의 Job은 별개다. 이전 개발 스냅샷의 직접 자식 종료에서 변경된 동작이며 현재 Windows amd64와 Go 1.25 이상에서 검증한다.

## 시작과 등록

최초 관리형 워커 생성 전에 런타임 자신을 소유자 Job에 등록한다. 이후 CreateProcess로 만든 자식은 생성 시점부터 소유자 Job의 구성원이다. 소유자 Job은 상속되지 않는 핸들 하나를 런타임 수명 동안 유지하고 OS 프로세스 종료 때 닫는다. 실행 중 명시적으로 닫으면 런타임도 종료되므로 정상 워커 정리에서는 닫지 않는다.

각 워커는 별도의 Job을 만든 뒤 CREATE_SUSPENDED로 시작한다. 워커 Job에 등록하고 주 스레드를 재개하므로 앱이 즉시 생성하는 자식도 워커 Job을 상속한다. 생성 후 워커 Job 등록 전에 런타임이 강제 종료되는 틈은 소유자 Job이 회수한다.

Job 생성·등록·재개가 실패하면 정지 자식을 종료·수거하고 실패를 기존 재시작 예산에 반영한다. 직접 자식 종료 방식으로 우회하지 않는다. 호스트의 기존 Job 제약이 중첩 등록을 허용하지 않으면 해당 관리형 워커가 시작하지 못할 수 있다. 로그의 Job 등록 오류를 확인한다.

## 종료와 재시작

두 종류의 Job 모두 KILL_ON_JOB_CLOSE를 설정한다. 일반적인 종료에서는 HTTP 요청을 먼저 drain한 뒤 워커 Job을 종료하고 직접 자식을 수거한다. 부모 워커가 먼저 종료돼도 남은 자손을 제거한 후 해당 세대의 Job을 닫는다. 종료 요청 전 Job 구성원의 프로세스 핸들을 확보하고, 관측한 구성원의 종료 신호와 Job의 활성 프로세스 수 0을 확인한 뒤 핸들을 닫는다. 단순 Job 핸들 닫기나 활성 수 0만으로 프로세스 종료 완료를 대신하지 않는다. 핸들 확보 시 Job 소속을 다시 확인해 PID 재사용으로 다른 프로세스를 기다리지 않는다.

정리 대기는 구성원 전체에 대해 최대 1초의 공통 예산을 사용한다. 스냅샷·종료 요청·대기·회계 조회·Job 핸들 닫기 중 오류가 있으면 `worker_resource_cleanup_failed`를 기록하고 해당 슬롯을 `failed`로 멈춘다. 이전 세대의 정리를 확인하지 못한 상태에서 새 세대의 예산을 겹치지 않는다. 직접 자식 수거 등 종료 절차 전체가 언제나 1초 안에 끝난다는 보장은 아니다. 프로세스 목록 스냅샷은 최대 65,536개로 제한하며 초과 시 정리 실패로 처리한다. 이 수는 앱 생성 상한이 아니며 실제 생성 수는 선택적 `processes_max`로 제한한다.

반복 정리는 이미 닫힌 핸들을 다시 사용하지 않고 이전 정리 오류도 보존한다. Job 설정, 실행 파일 시작, 일시 중지된 워커의 Job 연결·재개에 실패한 경로에서도 정리 오류를 호출자에게 보존한다. 정리를 확인할 수 없으면 worker_resource_cleanup_failed를 기록하고 해당 슬롯만 failed로 멈추며 새 세대를 만들지 않는다. 정리가 성공한 시작 오류는 기존 backoff·재시작 예산을 따른다.

런타임이 정상 정리 없이 종료되거나 TerminateProcess로 강제 종료돼도 OS가 Job 핸들을 닫아 소속 자손을 종료한다. 한 워커의 정상 재시작은 해당 Job만 종료하며 다른 Job의 워커를 종료하지 않는다.

Windows의 Job 종료는 강제 종료다. 앱의 종료 hook, finally, 버퍼 flush 실행을 보장하지 않는다. Linux·macOS의 SIGTERM 후 대기·SIGKILL과 구분해야 한다.

## 보장 범위

일반적인 CreateProcess 자손의 수명 관리다. WMI나 외부 서비스가 대신 만든 프로세스는 같은 Job에 속하지 않을 수 있다. 선택한 [메모리 예산](memory-limits.md)은 같은 워커 Job의 committed memory 합에 적용한다. 선택적 [CPU 상한](cpu-limits.md)은 같은 워커 Job에 적용한다. DB quota나 같은 사용자 권한의 악성 프로세스를 막는 샌드박스는 제공하지 않는다. 외부 `upstreams` 서버를 종료하거나 관리하지 않는다.

Linux·macOS는 [프로세스 그룹](process-groups.md)을 사용하며 그 밖의 OS는 현재 직접 자식 종료 범위다. 과거 검증 기록은 당시 구현 범위를 유지한다.

## 검증

```powershell
go test ./internal/supervisor -run 'TestWindows(Job|OwnerJob|ManagedCrash)' -count=10 -timeout=60s
go test ./... -count=1 -timeout=90s
go vet ./...
```

실제 테스트 프로세스가 즉시 자식을 생성한다. 해당 트리만 종료하며 별도 워커가 살아 있는지, 부모 워커 종료 후 자손이 Job을 닫을 때 제거되는지, 소유자의 정상·강제 종료가 자손을 제거하는지 검사한다. Job 등록 전 정지 워커만 남긴 소유자를 강제 종료하는 검사로 시작 중 회수도 확인한다. 실제 Supervisor 검사에서는 자손이 stdout/stderr까지 상속한 워커를 HTTP로 크래시시킨 뒤 이전 자손 회수·새 PID와 자손 생성·다른 그룹 유지·최종 Stop 회수를 검사한다. 재사용 가능한 PID 대신 미리 연 프로세스 핸들로 생존과 종료를 판정한다.

정리 완료 검사는 실제 워커·자손 트리에서 close 반환 직후 0ms 대기로 확인한다. 자손 20개를 만든 트리로 프로세스 목록 버퍼 확장 경로도 검사한다.

Microsoft의 [TerminateJobObject](https://learn.microsoft.com/en-us/windows/win32/api/jobapi2/nf-jobapi2-terminatejobobject), [Job 프로세스 목록](https://learn.microsoft.com/en-us/windows/win32/api/winnt/ns-winnt-jobobject_basic_process_id_list), [Job Objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects), [AssignProcessToJobObject](https://learn.microsoft.com/en-us/windows/win32/api/jobapi2/nf-jobapi2-assignprocesstojobobject), [프로세스 생성 플래그](https://learn.microsoft.com/en-us/windows/win32/procthread/process-creation-flags) 계약을 따른다.
