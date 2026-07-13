# hfdown

한국어 | [English](README.md)

Created by Jioh L. Jung <ziozzang@gmail.com>

`hfdown`은 Hugging Face의 모델과 데이터셋 리포지터리를 낮은 리소스로
다운로드하는 프로그램입니다. 중단된 다운로드 이어받기와 HTTP Range 분할
다운로드를 지원하며, 리비전을 Git 커밋으로 확정한 뒤 Git blob 또는 Git LFS
해시로 받은 파일을 검증합니다.

## 주요 기능

- 모델 및 데이터셋 리포지터리를 확정된 커밋 SHA 기준으로 다운로드
- 리포 전체 다운로드 또는 대소문자를 구분하지 않는 glob 파일 필터
- 기본 활성화된 이어받기와 HTTP Range 분할 다운로드
- 고정 크기 I/O 버퍼 사용; 파일 전체를 메모리에 올리지 않음
- 파일별 Git blob SHA-1 또는 Git LFS SHA-256 검증
- 받은 모든 파일의 raw SHA-256과 SHA-1을 `.sha256`, `.sha1sum`에 저장
- Hub 메타데이터 스냅샷과 누적 업데이트/검증 이력
- 단순 텍스트 목록과 작업별 설정이 가능한 JSON 큐
- 전체 디렉터리 순회 검증과 강제 전수 재해시
- 원격 객체 해시가 바뀐 파일만 받는 증분 업데이트
- macOS, Windows, Linux의 ARM64/x86-64 정적 바이너리

## 설치 및 빌드

[GitHub Releases](https://github.com/ziozzang/hfdownload/releases)에서 미리
빌드한 바이너리를 받거나 Go 1.24 이상으로 빌드합니다.

```bash
go install github.com/ziozzang/hfdownload/cmd/hfdown@latest

make build
./hfdown version
./hfdown --help
```

명령별 옵션은 `hfdown help COMMAND` 또는 `hfdown COMMAND --help`로 확인합니다.
`hfdown --version`, `hfdown -v`, `hfdown -V`도 버전 별칭으로 지원합니다.

6개 플랫폼 바이너리를 모두 만들려면 다음을 실행합니다.

```bash
make release
```

결과는 `dist/`에 생성됩니다. 릴리스 빌드는 `CGO_ENABLED=0`, `netgo`,
`osusergo`를 사용하며, 빌드 스크립트가 Linux 결과물의 정적 링크 여부도
확인합니다.

## 인증

기본적으로 `os.Getenv("HF_TOKEN")`으로 토큰을 읽습니다.

```bash
export HF_TOKEN=hf_xxx
hfdown d owner/model
```

다른 환경 변수나 직접 입력도 지원합니다.

```bash
hfdown d --token-env MY_HF_TOKEN owner/model
hfdown d --token hf_xxx owner/model
```

명령행 인자는 셸 기록이나 프로세스 목록에 노출될 수 있으므로 환경 변수
사용을 권장합니다. 토큰은 메타데이터, 체크섬, 설정 파일, 로그에 기록하지
않습니다.

## 모델 다운로드

`download`, `dn`, `d`는 같은 명령입니다. 리포 ID와 전체 URL을 모두 받을 수
있습니다. 기본 로컬 디렉터리 이름은 `<owner>_<repo>`입니다.

```bash
hfdown download FluidInference/silero-vad-coreml
hfdown dn FluidInference/silero-vad-coreml
hfdown d https://huggingface.co/FluidInference/silero-vad-coreml
# -> ./FluidInference_silero-vad-coreml/
```

저장 경로와 분할 수를 직접 지정할 수 있습니다.

```bash
hfdown d \
  --output ./models/silero-vad \
  --parts 8 \
  --multipart-threshold 64MiB \
  FluidInference/silero-vad-coreml
```

## 데이터셋 다운로드

`dataset`, `ds`는 같은 명령입니다. 데이터셋 ID와 `/datasets/`가 포함된 전체
URL을 모두 지원합니다. 기본 디렉터리 이름 역시 `<owner>_<repo>`입니다.

```bash
hfdown dataset lhoestq/demo1
hfdown ds https://huggingface.co/datasets/lhoestq/demo1
# -> ./lhoestq_demo1/
```

## 태그, 리비전, 파일 필터

`--revision`에는 브랜치, 태그, 커밋 SHA를 쓸 수 있습니다. `--tag`는 태그를
편하게 지정하는 옵션이며 `--revision`보다 우선합니다.

```bash
hfdown d --tag v1.2.0 owner/model
hfdown ds --revision 0123456789abcdef owner/dataset
```

`--filter`는 셸 glob 형태로 받을 파일을 선택합니다. 대소문자를 구분하지
않으며, 여러 패턴은 OR 조건입니다. 옵션을 반복하거나 `|`로 연결하거나 두
방식을 함께 쓸 수 있습니다.

```bash
hfdown d \
  --filter '*.json|*.parquet|*_q4_?.gguf' \
  owner/model

hfdown d \
  --filter '*.json' \
  --filter '*.gguf' \
  owner/model

hfdown d \
  --tag Q4-release \
  --filter 'weights/*_q4_?.gguf|tokenizer*.json' \
  owner/model
```

셸이 `*`, `?`, `|`를 먼저 처리하지 않도록 필터를 따옴표로 감싸십시오. `/`가
있는 패턴은 리포 기준 전체 상대 경로에, `/`가 없는 패턴은 모든 디렉터리의
파일명에 적용됩니다. 일치하는 파일이 하나도 없으면 조용히 끝내지 않고
오류로 알립니다.

필터를 사용한 업데이트는 현재 커밋을 다시 확인하되, 선택된 파일 중 원격
객체 해시가 변경된 것만 받습니다. 기존에 관리하던 미선택 파일은 manifest와
체크섬 파일에서 제거하지 않습니다.

## 주요 다운로드 옵션

- `--revision main`: 브랜치, 태그 또는 커밋 SHA
- `--tag TAG`: 태그 이름; `--revision`보다 우선
- `--filter GLOB`: 포함 필터; 반복하거나 `|`로 여러 패턴 지정
- `--parts 4`: 큰 파일의 병렬 Range 수; `1`이면 분할하지 않음
- `--multipart-threshold 64MiB`: 분할 다운로드를 시작할 최소 파일 크기
- `--resume=true`: 호환되는 임시 다운로드 이어받기
- `--buffer-size 1MiB`: 활성 Range 하나가 사용하는 메모리 버퍼
- `--retries 5`: Range별 재시도 횟수
- `--token-env HF_TOKEN`: 토큰을 담은 환경 변수 이름
- `--token TOKEN`: 토큰 직접 전달

기본 다운로드 버퍼 메모리는 대략 `parts × buffer-size`입니다. 파일은 한 번에
하나씩 처리하며 파일 전체를 메모리에 읽지 않습니다.

## 단순 텍스트 배치 목록

한 줄에 리포 ID 하나를 씁니다. 빈 줄과 공백을 제외한 첫 문자가 `;`인 줄은
주석으로 무시합니다.

```text
; 내려받을 모델

FluidInference/silero-vad-coreml
openai-community/gpt2
```

모델 목록을 받습니다.

```bash
hfdown batch --list models.txt
hfdown batch --list models.txt --output-root ./models --continue-on-error
```

같은 형식으로 데이터셋 목록을 받습니다.

```bash
hfdown batch --type dataset --list datasets.txt --output-root ./datasets
```

배치 목록 전체에 `--tag`, `--filter`를 적용할 수도 있습니다. 예시는
[`models.example.txt`](models.example.txt)를 참고하십시오.

## JSON 큐

모델과 데이터셋을 섞거나 작업별 옵션이 다르면 JSON 큐를 사용합니다.

```json
{
  "output_root": "./repositories",
  "jobs": [
    {
      "repo": "owner/model",
      "revision": "v2",
      "filters": ["*.json|*_q8_0.gguf"]
    },
    {
      "repo": "owner/dataset",
      "type": "dataset",
      "filters": ["*.parquet"]
    }
  ]
}
```

```bash
hfdown batch --queue queue.json
hfdown batch --queue queue.json --continue-on-error
```

작업별 상세 옵션은 [`queue.example.json`](queue.example.json)을 참고하십시오.

## 이어받기 동작

이어받기는 기본값으로 활성화되어 있습니다.

- 호환되는 `tmp/<file-key>/state.json`을 발견하면 각 Range의 저장된 위치부터
  이어받습니다.
- 최종 경로에 원격 파일보다 짧은 파일이 있으면 `tmp/`로 옮겨 기존 prefix로
  사용합니다.
- 여러 Range는 하나의 `download.part` 내 각 위치에 직접 기록하므로 파일
  크기만 한 별도 병합 복사본이 필요하지 않습니다.
- 전체 원격 해시가 일치해야 최종 경로로 이동합니다.
- 파일 하나가 성공할 때마다 manifest, `.sha256`, `.sha1sum`을 갱신하므로
  작업이 중단돼도 그때까지 완료한 모든 파일의 체크섬이 남습니다.
- 이어받은 데이터의 해시가 틀리면 같은 실행에서 해당 파일만 0부터 한 번
  다시 받습니다.
- 유효한 manifest 기록이 있고 변경되지 않은 파일은 다시 읽지 않습니다.
- 현재 검증 기록이 없는 파일은 다운로드 여부를 정하기 전에 한 번 검사합니다.

## 진행률 표시

전송 전에 선택된 파일 전체 계획을 출력합니다.

```text
plan: 42 files • 14.8 GiB total • 35 cached (9.2 GiB) • 7 remaining (5.6 GiB)
```

진행 바에는 현재 파일, 완료/전체 파일 수, 퍼센트, 바이트, 속도가 표시됩니다.
완료 요약에는 캐시된 파일과 용량, 검사로 재사용한 기존 파일, 실제 다운로드한
파일, 네트워크 수신량, 이어받아 재사용한 용량이 나옵니다.

## 검증과 업데이트

기본 검증은 변경되지 않은 파일의 기존 검증 기록을 재사용합니다.

```bash
hfdown verify --output ./FluidInference_silero-vad-coreml
```

모든 파일을 강제로 다시 읽거나 관리 중인 리포를 전수 검사할 수 있습니다.

```bash
hfdown verify --output ./FluidInference_silero-vad-coreml --force
hfdown verify-batch --root ./models --force
hfdown status --output ./FluidInference_silero-vad-coreml
```

`verify-batch`는 기본적으로 실패 후에도 다음 디렉터리를 검사합니다. 첫 실패에
중단하려면 `--fail-fast`를 추가합니다.

나중에 같은 다운로드 명령이나 배치 큐를 다시 실행하면 요청 리비전을 새로
확정합니다. Git blob 또는 Git LFS 객체 해시를 비교하여 변경된 파일만 다시
받습니다. 리비전에 커밋 SHA를 직접 지정하면 그 커밋에 고정됩니다.

## 저장 구조

```text
<repository-directory>/
├── <downloaded files>
├── .sha256
├── .sha1sum
├── .metadata/
│   ├── manifest.json
│   ├── repository.json
│   ├── repository-history.jsonl
│   └── verification-history.jsonl
└── tmp/                         # 다운로드가 끝나지 않았을 때만 존재
    └── <file-key>/
        ├── download.part
        └── state.json
```

`tmp/`는 의도적으로 숨김 디렉터리가 아닙니다. `tmp/`, `.metadata/`,
`.sha256`, `.sha1sum`은 `hfdown` 전용 경로이며 원격 리포에 같은 경로가
있으면 충돌 방지를 위해 중단합니다. 예전 `hfdown-metadata/`, `.hfdown/`, `.metadata/tmp/`,
`.hfdown/partials/` 구조는 자동으로 이전합니다.

## 해시와 메타데이터 정책

- 다운로드 전에 요청 리비전을 커밋 SHA로 확정합니다.
- 모든 파일 URL에 확정된 커밋을 사용합니다.
- 일반 Git 파일은 Git blob SHA-1(`blob <size>\0<content>`)로 검사합니다.
- Git LFS 파일은 Hub가 제공한 LFS SHA-256으로 검사합니다.
- 관리하는 모든 로컬 파일에 raw-content SHA-256과 SHA-1도 별도로 기록합니다.
- `.sha1sum`의 raw SHA-1은 Git 객체 헤더까지 포함해 계산하는 Git blob SHA-1과
  다르며, Git blob SHA-1은 manifest에 별도로 기록합니다.

다운로드 또는 검증 성공 후 두 파일은 표준 coreutils 형식으로 저장됩니다.

```bash
cd ./FluidInference_silero-vad-coreml
sha256sum -c .sha256
sha1sum -c .sha1sum
```

`.metadata/repository.json`은 최신 Hub API 응답, 리포 종류, 요청 리비전,
확정 커밋, 조회 시각, 생성 시각, Hub 최종 수정 시각을 저장합니다.
`repository-history.jsonl`에는 메타데이터 확인 이력을 누적합니다.
`manifest.json`에는 원격/로컬 해시와 검증 상태를,
`verification-history.jsonl`에는 검증 실행 요약을 저장합니다.

## 설정 파일

현재 디렉터리에 `.hfdown.json`이 있으면 자동으로 읽습니다. 다른 파일은
`--config FILE`로 선택합니다. 명령행의 단일 값 옵션은 설정 파일보다
우선합니다.

```json
{
  "endpoint": "https://huggingface.co",
  "revision": "main",
  "output": "",
  "filters": ["*.json|*.gguf"],
  "parts": 4,
  "multipart_threshold": 67108864,
  "buffer_size": 1048576,
  "retries": 5,
  "timeout_seconds": 30,
  "resume": true,
  "token_env": "HF_TOKEN"
}
```

설정 파일에는 토큰을 저장하지 말고 `token_env` 또는 `--token`을 사용하십시오.

## 라이선스

MIT. [`LICENSE`](LICENSE)를 참고하십시오.
