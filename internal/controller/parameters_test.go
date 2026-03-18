package controller

import (
	"testing"

	"github.com/onsi/gomega"
)

func TestCalculatePostgresParameters_CoordinatorFormulas(t *testing.T) {
	g := gomega.NewWithT(t)

	// 24Gi memory = 25769803776 bytes, 3 CPU cores
	memBytes := int64(24 * 1024 * 1024 * 1024)
	cpuCores := int64(3)

	params := map[string]string{
		"shared_buffers":                   `{{ div (div .memory 3) 8192 }}`,
		"temp_buffers":                     `{{ div (div .memory 64) 8192 }}`,
		"work_mem":                         `{{ div (div .memory 256) 1024 }}`,
		"effective_cache_size":             `{{ div (div (mul .memory 3) 4) 1024 }}kB`,
		"maintenance_work_mem":             `{{ div (div .memory 32) 1024 }}`,
		"max_worker_processes":             `{{ max 24 (add (div .cpu 2) .cpu) }}`,
		"max_parallel_workers":             `{{ max 4 (div .cpu 4) }}`,
		"max_parallel_workers_per_gather":  `{{ div .cpu 2 }}`,
		"max_parallel_maintenance_workers": `{{ div .cpu 2 }}`,
		"_test_cpu_identity":               `{{ .cpu }}`,
		"_test_memory_identity":            `{{ .memory }}`,
	}

	result, err := CalculatePostgresParameters(params, memBytes, cpuCores)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result).To(gomega.HaveLen(11))

	// Identity assertions — catch any change to inputs immediately.
	g.Expect(result["_test_cpu_identity"]).To(gomega.Equal("3"))
	g.Expect(result["_test_memory_identity"]).To(gomega.Equal("25769803776"))

	// shared_buffers: div(div(25769803776, 3), 8192) = 1048576
	g.Expect(result["shared_buffers"]).To(gomega.Equal("1048576"))

	// temp_buffers: div(div(25769803776, 64), 8192) = 49152
	g.Expect(result["temp_buffers"]).To(gomega.Equal("49152"))

	// work_mem: div(div(25769803776, 256), 1024) = 98304
	g.Expect(result["work_mem"]).To(gomega.Equal("98304"))

	// effective_cache_size: div(div(mul(25769803776, 3), 4), 1024) = 18874368
	g.Expect(result["effective_cache_size"]).To(gomega.Equal("18874368kB"))

	// maintenance_work_mem: div(div(25769803776, 32), 1024) = 786432
	g.Expect(result["maintenance_work_mem"]).To(gomega.Equal("786432"))

	// max_worker_processes: max(24, add(div(3, 2), 3)) = max(24, 4) = 24
	g.Expect(result["max_worker_processes"]).To(gomega.Equal("24"))

	// max_parallel_workers: max(4, div(3, 4)) = max(4, 0) = 4
	g.Expect(result["max_parallel_workers"]).To(gomega.Equal("4"))

	// max_parallel_workers_per_gather: div(3, 2) = 1
	g.Expect(result["max_parallel_workers_per_gather"]).To(gomega.Equal("1"))

	// max_parallel_maintenance_workers: div(3, 2) = 1
	g.Expect(result["max_parallel_maintenance_workers"]).To(gomega.Equal("1"))
}

func TestCalculatePostgresParameters_PBFormulas(t *testing.T) {
	g := gomega.NewWithT(t)

	// 24Gi memory, 8 CPU cores
	memBytes := int64(24 * 1024 * 1024 * 1024)
	cpuCores := int64(8)

	params := map[string]string{
		"shared_buffers":                  `{{ div (div .memory 4) 8192 }}`,
		"temp_buffers":                    `{{ div (div .memory 128) 8192 }}`,
		"work_mem":                        `{{ div (div .memory 128) 1024 }}`,
		"max_worker_processes":            `{{ .cpu }}`,
		"max_parallel_workers":            `{{ .cpu }}`,
		"max_parallel_workers_per_gather": `{{ div .cpu 2 }}`,
		"max_parallel_maintenance_workers": `{{ div .cpu 2 }}`,
	}

	result, err := CalculatePostgresParameters(params, memBytes, cpuCores)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// shared_buffers: div(div(24*1024*1024*1024, 4), 8192) = div(6442450944, 8192) = 786432
	g.Expect(result["shared_buffers"]).To(gomega.Equal("786432"))

	// max_worker_processes: just .cpu = 8
	g.Expect(result["max_worker_processes"]).To(gomega.Equal("8"))

	// max_parallel_workers: just .cpu = 8
	g.Expect(result["max_parallel_workers"]).To(gomega.Equal("8"))

	// max_parallel_workers_per_gather: div(8, 2) = 4
	g.Expect(result["max_parallel_workers_per_gather"]).To(gomega.Equal("4"))
}

func TestCalculatePostgresParameters_StaticValues(t *testing.T) {
	g := gomega.NewWithT(t)

	params := map[string]string{
		"shared_buffers":     `{{ div (div .memory 3) 8192 }}`,
		"max_connections":    "300",
		"statement_timeout":  "8h",
		"auto_explain.log_analyze": "off",
	}

	memBytes := int64(16 * 1024 * 1024 * 1024)
	cpuCores := int64(2)

	result, err := CalculatePostgresParameters(params, memBytes, cpuCores)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Static values pass through unchanged.
	g.Expect(result["max_connections"]).To(gomega.Equal("300"))
	g.Expect(result["statement_timeout"]).To(gomega.Equal("8h"))
	g.Expect(result["auto_explain.log_analyze"]).To(gomega.Equal("off"))

	// Template value is evaluated.
	g.Expect(result["shared_buffers"]).NotTo(gomega.BeEmpty())
}

func TestCalculatePostgresParameters_EmptyMap(t *testing.T) {
	g := gomega.NewWithT(t)

	result, err := CalculatePostgresParameters(nil, 1024, 1)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result).To(gomega.BeNil())

	result, err = CalculatePostgresParameters(map[string]string{}, 1024, 1)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result).To(gomega.BeNil())
}

func TestCalculatePostgresParameters_InvalidTemplate(t *testing.T) {
	g := gomega.NewWithT(t)

	params := map[string]string{
		"bad_param": `{{ invalid_func .memory }}`,
	}

	_, err := CalculatePostgresParameters(params, 1024, 1)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("bad_param"))
}

func TestCalculatePostgresParameters_TemplateFunctions(t *testing.T) {
	g := gomega.NewWithT(t)

	params := map[string]string{
		"test_div": `{{ div .memory 1024 }}`,
		"test_mul": `{{ mul .cpu 100 }}`,
		"test_add": `{{ add .cpu 10 }}`,
		"test_max": `{{ max .cpu 16 }}`,
	}

	result, err := CalculatePostgresParameters(params, 10240, 4)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	g.Expect(result["test_div"]).To(gomega.Equal("10"))    // 10240 / 1024
	g.Expect(result["test_mul"]).To(gomega.Equal("400"))   // 4 * 100
	g.Expect(result["test_add"]).To(gomega.Equal("14"))    // 4 + 10
	g.Expect(result["test_max"]).To(gomega.Equal("16"))    // max(4, 16)
}

func TestCalculatePostgresParameters_ZeroCPU(t *testing.T) {
	g := gomega.NewWithT(t)

	params := map[string]string{
		"max_worker_processes": `{{ max 4 .cpu }}`,
	}

	result, err := CalculatePostgresParameters(params, 1024*1024*1024, 0)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(result["max_worker_processes"]).To(gomega.Equal("4"))
}

func TestCalculatePostgresParameters_LargeValues(t *testing.T) {
	g := gomega.NewWithT(t)

	// 256Gi memory, 64 cores
	memBytes := int64(256 * 1024 * 1024 * 1024)
	cpuCores := int64(64)

	params := map[string]string{
		"shared_buffers":       `{{ div (div .memory 3) 8192 }}`,
		"max_worker_processes": `{{ max 24 (add (div .cpu 2) .cpu) }}`,
	}

	result, err := CalculatePostgresParameters(params, memBytes, cpuCores)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// shared_buffers: div(div(274877906944, 3), 8192) = div(91625968981, 8192) = 11184810
	g.Expect(result["shared_buffers"]).To(gomega.Equal("11184810"))

	// max_worker_processes: max(24, add(32, 64)) = max(24, 96) = 96
	g.Expect(result["max_worker_processes"]).To(gomega.Equal("96"))
}

func TestCalculatePostgresParameters_WorkerFormulas(t *testing.T) {
	g := gomega.NewWithT(t)

	// 16Gi memory, 3 CPU cores — worker node formulas from helm chart
	memBytes := int64(16 * 1024 * 1024 * 1024)
	cpuCores := int64(3)

	params := map[string]string{
		"work_mem":             `{{ div (div .memory 512) 1024 }}`,
		"max_worker_processes": `{{ add (add (div .cpu 4) .cpu) 8 }}`,
		"max_parallel_workers": `{{ div .cpu 2 }}`,
	}

	result, err := CalculatePostgresParameters(params, memBytes, cpuCores)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// work_mem: div(div(17179869184, 512), 1024) = 32768
	g.Expect(result["work_mem"]).To(gomega.Equal("32768"))

	// max_worker_processes: add(add(div(3, 4), 3), 8) = add(add(0, 3), 8) = 11
	g.Expect(result["max_worker_processes"]).To(gomega.Equal("11"))

	// max_parallel_workers: div(3, 2) = 1
	g.Expect(result["max_parallel_workers"]).To(gomega.Equal("1"))
}
