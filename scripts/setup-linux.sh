#!/usr/bin/env bash
# Run from a source checkout on the Linux machine being installed.
set -euo pipefail
engine='' inference='' model='' check=0 bin_dir=''
usage() {
  cat <<'EOF'
Usage: sudo bash scripts/setup-linux.sh [options]

Without options, asks for container engine, inference engine and model.
  --engine docker|podman              Rootful local engine (must be installed)
  --inference ollama|llama.cpp|vllm|sglang|none
  --model NAME                       Ollama tag, Hugging Face repo, or GGUF path
  --bin-dir DIRECTORY                Use prebuilt native Linux Cell binaries
  --check                            Read-only prerequisite checks; no downloads
  --help

Ollama/llama.cpp profiles use CPU. vLLM/SGLang profiles require NVIDIA GPU,
CUDA 13-compatible drivers and configured NVIDIA Container Toolkit.
The installer never installs OS packages, a container engine, or GPU drivers.
EOF
}
while (($#)); do
  case "$1" in
    --engine|--inference|--model|--bin-dir)
      (($# >= 2)) || { echo "Missing value for $1" >&2; exit 2; }
      case "$1" in
        --engine) engine=$2;; --inference) inference=$2;;
        --model) model=$2;; --bin-dir) bin_dir=$2;;
      esac
      shift 2;;
    --check) check=1; shift;;
    --help|-h) usage; exit 0;;
    *) echo "Unknown option: $1" >&2; usage; exit 2;;
  esac
done
choose() {
  [[ -t 0 ]] || { echo "Noninteractive setup requires --engine and --inference." >&2; exit 2; }
  read -r -p "$1" REPLY || exit 2
}
if [[ -z $engine ]]; then
  echo 'Container engine: 1) Docker  2) Podman'
  choose 'Choose [1/2]: '
  case $REPLY in 1|docker|Docker) engine=docker;; 2|podman|Podman) engine=podman;; *) echo 'Invalid engine choice.' >&2; exit 2;; esac
fi
case $engine in docker|podman) ;; *) echo 'Use --engine docker or podman.' >&2; exit 2;; esac
if [[ -z $inference ]]; then
  echo 'Inference engine: 1) Ollama (CPU)  2) llama.cpp (CPU)  3) vLLM (NVIDIA GPU)  4) SGLang (NVIDIA GPU)  5) Skip local inference'
  choose 'Choose [1-5]: '
  case $REPLY in 1|ollama) inference=ollama;; 2|llama.cpp) inference=llama.cpp;; 3|vllm) inference=vllm;; 4|sglang) inference=sglang;; 5|none) inference=none;; *) echo 'Invalid inference choice.' >&2; exit 2;; esac
fi
case $inference in ollama|llama.cpp|vllm|sglang|none) ;; *) echo 'Unsupported inference engine.' >&2; exit 2;; esac
if [[ -z $model && $inference != none && -t 0 ]]; then
  case $inference in
    ollama) default_model='qwen3:0.6b';;
    llama.cpp) default_model='Qwen3-0.6B-Q8_0 (verified download)';;
    *) default_model='Qwen/Qwen3-0.6B';;
  esac
  read -r -p "Model [$default_model] (llama.cpp: optional local GGUF path): " model || exit 2
fi
[[ $(uname -s) == Linux ]] || { echo 'Run this script inside the Linux VM, not on macOS.' >&2; exit 1; }
[[ $EUID == 0 ]] || { echo 'Run with sudo on the target Linux machine.' >&2; exit 1; }
command -v python3 >/dev/null || { echo '[FAIL] python3 is missing. Install it yourself, then rerun.' >&2; exit 1; }
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
exec python3 "$script_dir/linux-setup.py" --engine "$engine" --inference "$inference" --model "$model" --check "$check" --bin-dir "$bin_dir"
