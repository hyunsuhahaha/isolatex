# 배포 묶음 검증

아직 정식 릴리스 태그를 발행하지 않는다. 이 문서는 로컬·수동 CI artifact를 빌드하고 검증하는 절차다. 정식 버전의 안정성이나 모든 OS에서 실제 실행을 검증했다는 뜻은 아니다.

## 빌드

Linux(WSL2 포함) 또는 macOS에서 Go 1.25 이상, Git, Bash, tar와 `sha256sum` 또는 `shasum`이 필요하다. 빌드 스크립트는 사용 가능한 SHA-256 도구를 선택한다.

```sh
bash scripts/build-dist.sh dev
```

기본 대상은 Linux amd64/arm64, Windows amd64, macOS amd64/arm64다. 각 묶음은 `bin/isolatex`, `bin/demo-app`(Windows는 `.exe`), 기본 설정, 라이선스, README와 문서를 포함한다. 버전 이름은 첫 인자로 지정한다. `-version`의 커밋에 `-dirty`가 붙으면 커밋되지 않은 작업 상태에서 빌드한 것이다.

일부 대상만 만들려면 다음처럼 지정한다.

```sh
TARGETS='linux/amd64 windows/amd64' bash scripts/build-dist.sh dev
```

`dist/SHA256SUMS`는 이번 실행에서 만든 압축 파일의 SHA-256 목록이다. 체크섬은 파일 손상 검증용이며 배포자 서명은 아니다.

## 실행 확인

```sh
cd dist
sha256sum -c SHA256SUMS
mkdir -p unpacked
tar -xzf isolatex_dev_linux_amd64.tar.gz -C unpacked
cd unpacked
bin/isolatex -version
bin/isolatex -config isolatex.yaml -check
bin/isolatex -config isolatex.yaml
```

별도 터미널에서 `/a/work`, `/b/work`와 관리 포트의 `/readyz`, `/metrics`를 확인한다. 묶음의 루트가 현재 작업 디렉터리여야 `./bin/demo-app` 명령을 찾을 수 있다. Ctrl+C로 drain·워커 종료를 확인한다.

macOS에서는 체크섬 명령을 `shasum -a 256 -c SHA256SUMS`로 바꾼다. 압축 파일 이름은 실행할 기기의 OS·아키텍처와 일치해야 한다.

현재 저장소의 Linux/macOS 네이티브 묶음은 저장소 루트에서 다음과 같이 양방향 장애 검사를 실행할 수 있다. Go와 curl이 필요하며 기본 설정의 포트가 비어 있어야 한다.

```sh
bash scripts/smoke-dist.sh dist/unpacked
```

이 도구는 HTTP 부하 driver만 소스에서 빌드한다. 런타임과 데모 워커는 압축 해제 폴더에서 실행하며, 상대 명령 경로·포화·워커 크래시 복구·보호 그룹의 응답과 PID 유지·런타임의 정상 종료를 검사한다. 자신이 시작한 관리 PID를 확인하므로 `/status.runtime_pid`가 없는 이전 스냅샷은 검사에 실패한다. 두 선택 인자는 gateway·admin origin이며, 추출본 YAML의 주소와 맞춰야 한다. 검사가 성공하면 종료와 임시 파일 정리를 마친 뒤 `native_distribution_smoke_passed`를 출력한다.

Windows에서는 압축을 푼 폴더에서 PowerShell로 `./bin/isolatex.exe -version`과 `./bin/isolatex.exe -config isolatex.yaml -check`를 실행한다. 현재 소스의 관리형 워커는 Job Objects로 자손 수명을 관리한다. 이전 개발 스냅샷은 직접 자식 종료만 지원하므로 버전의 커밋과 해당 문서를 확인한다. OS별 종료 방식은 [Windows 프로세스 수명](windows-processes.md)에 구분한다.

수동 GitHub `distribution` 워크플로는 전체 테스트 후 같은 빌드 스크립트를 실행하고 체크섬·Linux amd64 버전·설정 검사를 수행한다. 일반 `test` 워크플로의 stable Linux amd64/arm64·macOS amd64/arm64 행은 각 기기용 묶음을 만들어 압축 해제·버전·설정·양방향 장애 검사를 수행한다. 실제 실행 결과는 [검증 기록](verification.md)에 기록하며 크로스 컴파일 성공과 구분한다.
