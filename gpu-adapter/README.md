<a id="readme-top"></a>

[![Go][go-shield]][go-url]
[![Kubernetes DRA][dra-shield]][dra-url]
[![KubeVirt][kubevirt-shield]][kubevirt-url]
[![OpenAPI][openapi-shield]][openapi-url]

<br />
<div align="center">
  <h3 align="center">gpu-adapter</h3>

  <p align="center">
    GPU 노드의 GPU를 나누고, 지정한 GPU로 컨테이너와 VM을 실행하는 REST API 서버
    <br />
  </p>
</div>

<details>
  <summary>목차</summary>
  <ol>
    <li>
      <a href="#프로젝트-소개">프로젝트 소개</a>
      <ul>
        <li><a href="#주요-기능">주요 기능</a></li>
        <li><a href="#사용-기술">사용 기술</a></li>
      </ul>
    </li>
    <li>
      <a href="#시작하기">시작하기</a>
      <ul>
        <li><a href="#사전-요구사항">사전 요구사항</a></li>
        <li><a href="#빌드">빌드</a></li>
        <li><a href="#설정">설정</a></li>
      </ul>
    </li>
    <li>
      <a href="#사용법">사용법</a>
      <ul>
        <li><a href="#api-요약">API 요약</a></li>
      </ul>
    </li>
  </ol>
</details>

## 프로젝트 소개

gpu-adapter는 Kubernetes GPU 노드마다 하나씩 실행되는 REST API 서버입니다. 호출자는 이 API 하나로 노드의 GPU를 조회하고, FULL, MIG, TIME_SLICING 중 원하는 방식으로 나누고, 지정한 GPU로 컨테이너 및 VM 워크로드를 생성합니다.

### 주요 기능

* **GPU 조회**: NVML 기반으로 GPU 정보를 제공합니다.
* **GPU 재구성**: FULL, MIG, TIME_SLICING 사이를 전환하고 MIG 인스턴스를 만들거나 지웁니다.
* **이기종 GPU**: DRA 기반으로 단일 노드 내 이기종 GPU 구성 및 워크로드 할당을 제공합니다.
* **컨테이너 워크로드**: `service`(Deployment)와 `job`(Job)을 FULL GPU, MIG 인스턴스, TIME_SLICING GPU 유형으로 실행합니다.
* **VM 워크로드**: KubeVirt VM에 FULL GPU를 vfio로 할당합니다. cloud-init user-data와 볼륨, 포트를 지원합니다.
* **볼륨**: 워크로드와 별개로 유지되는 PVC를 만들고, 쓰고 있는 볼륨은 지우지 못하게 막습니다.

### 사용 기술

* [![Go][go-badge]][go-url]
* [![Gin][gin-badge]][gin-url]
* [![Kubernetes][k8s-badge]][client-go-url]
* [![KubeVirt][kubevirt-badge]][kubevirt-url]
* [![NVIDIA NVML][nvml-badge]][nvml-url]
* [![oapi-codegen][oapi-badge]][oapi-url]

## 시작하기

### 사전 요구사항

* Go 1.26 이상과 C 컴파일러 (NVML 바인딩용 cgo)
* `resource.k8s.io/v1` DRA를 지원하는 Kubernetes 클러스터
* NVIDIA DRA 드라이버
* VM 생성을 위한 KubeVirt
* 볼륨 생성을 위한 기본 StorageClass

### 빌드

1. 저장소를 받습니다.
   ```sh
   git clone https://github.com/GRIM-GPU/polaris.git
   cd polaris/gpu-adapter
   ```
2. 명세에서 서버 코드를 생성합니다. oapi-codegen은 `go.mod`의 tool로 고정되어 있어 따로 설치할 필요가 없습니다.
   ```sh
   go generate ./...
   ```
3. 빌드합니다.
   ```sh
   CGO_CFLAGS=-Wno-deprecated-declarations go build -o gpu-adapter ./cmd/gpu-adapter
   ```
4. lint를 실행합니다.
   ```sh
   golangci-lint run
   ```

### 설정

플래그, 환경 변수(`GPU_ADAPTER_` 접두어, `-`는 `_`), 설정 파일(`--config`, YAML/JSON/TOML) 중 어느 것으로도 지정할 수 있습니다.

| 플래그 | 환경 변수 | 기본값 | 설명 |
| --- | --- | --- | --- |
| `--api-token` | `GPU_ADAPTER_API_TOKEN` | (필수) | 호출자가 보낼 Bearer 토큰 |
| `--node-name` | `GPU_ADAPTER_NODE_NAME` | (필수) | 이 adapter가 관리하는 노드 이름 |
| `--pod-namespace` | `GPU_ADAPTER_POD_NAMESPACE` | (필수) | TIME_SLICING 기록 ConfigMap을 둘 namespace |
| `--nvml-library` | `GPU_ADAPTER_NVML_LIBRARY` | 기본 검색 경로 | `libnvidia-ml.so.1` 경로 |
| `--dra-plugin-namespace` | `GPU_ADAPTER_DRA_PLUGIN_NAMESPACE` | `gpu-operator` | DRA kubelet plugin Pod의 namespace |
| `--dra-plugin-selector` | `GPU_ADAPTER_DRA_PLUGIN_SELECTOR` | `app=nvidia-dra-driver-kubelet-plugin` | DRA kubelet plugin Pod의 라벨 선택자 |
| `--listen-addr` | `GPU_ADAPTER_LISTEN_ADDR` | `:8080` | API 수신 주소 |
| `--log-level` | `GPU_ADAPTER_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `--config` | `GPU_ADAPTER_CONFIG` | 없음 | 설정 파일 경로 |

## 사용법

### API 요약

| 경로 | 메서드 | 동작 |
| --- | --- | --- |
| `/healthz` | GET | 상태 확인 |
| `/v1/gpus` | GET | GPU 목록 |
| `/v1/gpus/{gpuRef}` | GET | GPU 상세 |
| `/v1/gpus/{gpuRef}/partition` | POST | 분할 변경 |
| `/v1/containers` | GET, POST | 컨테이너 워크로드 목록, 생성 |
| `/v1/containers/{namespace}/{name}` | GET, DELETE | 상세, 삭제 |
| `/v1/containers/{namespace}/{name}/start` | POST | 멈춘 워크로드를 지정한 GPU로 다시 시작 |
| `/v1/vms` | GET, POST | VM 목록, 생성 |
| `/v1/vms/{namespace}/{name}` | GET, PATCH, DELETE | 상세, GPU 변경, 삭제 |
| `/v1/vms/{namespace}/{name}/stop` | POST | VM 중지 |
| `/v1/vms/{namespace}/{name}/start` | POST | VM 시작 |
| `/v1/volumes` | GET, POST | 볼륨 목록, 생성 |
| `/v1/volumes/{namespace}/{name}` | DELETE | 볼륨 삭제 |

워크로드 상태(`status.state`)는 `PENDING`, `RUNNING`, `SUSPENDED`, `COMPLETED`, `FAILED` 중 하나입니다.
실패한 요청은 `code`와 `message`를 담아 응답합니다.

| `code` | 의미 |
| --- | --- |
| `INVALID_SPEC` | 요청 형식이 명세나 입력 규칙과 다름 |
| `NOT_FOUND` | GPU, 워크로드, 볼륨이 없음 |
| `CONFLICT` | 같은 이름의 워크로드나 볼륨이 이미 있음 |
| `INVALID_STATE` | 현재 상태에서 할 수 없는 요청 |
| `NOT_SUPPORTED` | GPU가 요청한 분할 방식이나 VM 할당을 지원하지 않음 |
| `DEVICE_UNAVAILABLE` | 지정한 GPU나 MIG 인스턴스가 없거나 분할 방식이 다름 |
| `DEVICE_BUSY` | 다른 워크로드나 요청이 GPU를 쓰는 중 |
| `PARTITION_FAILED` | GPU 분할 변경 실패 |
| `KUBERNETES_ERROR` | Kubernetes API 오류 |
| `TIMEOUT` | Kubernetes API 요청 시간 초과 |


[go-shield]: https://img.shields.io/badge/Go-1.26-00ADD8?style=for-the-badge&logo=go&logoColor=white
[dra-shield]: https://img.shields.io/badge/Kubernetes-DRA%20v1-326CE5?style=for-the-badge&logo=kubernetes&logoColor=white
[kubevirt-shield]: https://img.shields.io/badge/KubeVirt-VM-00AAB2?style=for-the-badge
[openapi-shield]: https://img.shields.io/badge/OpenAPI-3.0-6BA539?style=for-the-badge&logo=openapiinitiative&logoColor=white
[go-badge]: https://img.shields.io/badge/Go-00ADD8?style=for-the-badge&logo=go&logoColor=white
[gin-badge]: https://img.shields.io/badge/Gin-008ECF?style=for-the-badge&logo=gin&logoColor=white
[k8s-badge]: https://img.shields.io/badge/client--go-326CE5?style=for-the-badge&logo=kubernetes&logoColor=white
[kubevirt-badge]: https://img.shields.io/badge/KubeVirt-00AAB2?style=for-the-badge
[nvml-badge]: https://img.shields.io/badge/NVML-76B900?style=for-the-badge&logo=nvidia&logoColor=white
[oapi-badge]: https://img.shields.io/badge/oapi--codegen-6BA539?style=for-the-badge&logo=openapiinitiative&logoColor=white
[go-url]: https://go.dev/
[dra-url]: https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/
[kubevirt-url]: https://kubevirt.io/
[openapi-url]: ../openapi/gpu-adapter.openapi.yaml
[gin-url]: https://gin-gonic.com/
[client-go-url]: https://github.com/kubernetes/client-go
[nvml-url]: https://developer.nvidia.com/management-library-nvml
[oapi-url]: https://github.com/oapi-codegen/oapi-codegen
