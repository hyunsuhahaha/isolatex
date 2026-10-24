# 배포 묶음 검증

아직 정식 릴리스 태그를 발행하지 않는다. 이 문서는 로컬·수동 CI artifact를 빌드하고 검증하는 절차다. 정식 버전의 안정성이나 모든 OS에서 실제 실행을 검증했다는 뜻은 아니다.

## 빌드

Linux 또는 WSL2에서 Go 1.25 이상, Git, Bash, tar, sha256sum이 필요하다.

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

Windows에서는 압축을 푼 폴더에서 PowerShell로 `./bin/isolatex.exe -version`과 `./bin/isolatex.exe -config isolatex.yaml -check`를 실행한다. 현재 소스의 관리형 워커는 Job Objects로 자손 수명을 관리한다. 이전 개발 스냅샷은 직접 자식 종료만 지원하므로 버전의 커밋과 해당 문서를 확인한다. OS별 종료 방식은 [Windows 프로세스 수명](windows-processes.md)에 구분한다.

수동 GitHub `distribution` 워크플로는 전체 테스트 후 같은 스크립트를 실행하고 체크섬·Linux amd64 버전·설정 검사를 수행한다. 나머지 대상의 크로스 컴파일 성공과 실제 기기 실행 검증은 별도로 기록한다.
