package service

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/format"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

// The original's import plugins ([image_import_opts] image_import_plugins),
// run in order on the staged data before it is stored.

func (m *Importer) runPlugins(ctx context.Context, who auth.Identity, id string) error {
	for _, p := range m.cfg.Plugins {
		var err error
		switch p {
		case "no_op":
		case "image_decompression":
			err = m.decompress(id)
		case "image_conversion":
			err = m.convert(ctx, id)
		case "inject_image_metadata":
			err = m.inject(ctx, who, id)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *Importer) setImage(ctx context.Context, id string, change func(*model.Image)) error {
	_, err := m.svc.repo.Update(ctx, id, func(i model.Image) (model.Image, error) {
		change(&i)
		i.UpdatedAt = m.now().UTC()
		return i, nil
	})
	return err
}

// inject is inject_image_metadata: the configured properties, unless the
// user has one of ignore_user_roles.
func (m *Importer) inject(ctx context.Context, who auth.Identity, id string) error {
	if len(m.cfg.Inject) == 0 {
		return nil
	}
	for _, r := range who.Roles {
		if slices.Contains(m.cfg.InjectIgnore, r) {
			return nil
		}
	}
	return m.setImage(ctx, id, func(i *model.Image) {
		i.Properties = cloneProps(i.Properties)
		for k, v := range m.cfg.Inject {
			i.Properties[k] = v
		}
	})
}

func cloneProps(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// decompress is image_decompression: zip (one member) and gzip staged data
// is replaced by its content.
func (m *Importer) decompress(id string) error {
	path := m.staged(id)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	_ = f.Close()
	head = head[:n]
	tmp := path + ".decompressed"
	switch {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		z, err := zip.OpenReader(path)
		if err != nil {
			return errors.New("Error while extracting zip file")
		}
		defer z.Close()
		if len(z.File) != 1 {
			return errors.New("Zip contains more than one file. Only one file is supported")
		}
		r, err := z.File[0].Open()
		if err != nil {
			return errors.New("Error while extracting zip file")
		}
		defer r.Close()
		if err = writeFile(tmp, r); err != nil {
			return err
		}
	case bytes.HasPrefix(head, []byte("\x1f\x8b")):
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		g, err := gzip.NewReader(f)
		if err != nil {
			return errors.New("Error while extracting gzip file")
		}
		if err = writeFile(tmp, g); err != nil {
			return err
		}
	default:
		return nil
	}
	return os.Rename(tmp, path)
}

func writeFile(path string, r io.Reader) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	return errors.Join(err, out.Close())
}

// convert is image_conversion: the staged data must be its disk_format
// (inspected, then qemu-img info), have no backing or data file and an
// allowed VMDK create-type; qemu-img converts it to output_format.
func (m *Importer) convert(ctx context.Context, id string) error {
	image, err := m.svc.repo.Get(ctx, id)
	if err != nil {
		return err
	}
	src := m.staged(id)
	source := ""
	if image.DiskFormat != nil {
		source = *image.DiskFormat
	}
	target := m.cfg.ConversionFormat
	detected, err := format.DetectFile(src, m.cfg.GPTNonfatal)
	if err != nil {
		return err
	}
	if detected == "gpt" {
		detected = "raw"
	}
	if detected == "iso" {
		if source == "iso" {
			return nil // ISOs are not converted
		}
		return errors.New("Image has disallowed configuration")
	} else if detected != source {
		return errors.New("Image format mismatch")
	}
	run, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(run, "qemu-img", "info", "-f", source, "--output=json", src).Output()
	if err != nil {
		return errors.New("qemu-img info failed")
	}
	var info struct {
		Format   string `json:"format"`
		Backing  string `json:"backing-filename"`
		Specific struct {
			Data struct {
				DataFile   *string `json:"data-file"`
				CreateType string  `json:"create-type"`
			} `json:"data"`
		} `json:"format-specific"`
	}
	if json.Unmarshal(out, &info) != nil || info.Format != source {
		return errors.New("Image metadata disagrees about format")
	}
	if info.Backing != "" {
		return errors.New("QCOW images with backing files are not allowed")
	}
	if info.Specific.Data.DataFile != nil {
		return errors.New("QCOW images with data-file set are not allowed")
	}
	if info.Format == "vmdk" {
		if info.Specific.Data.CreateType == "" {
			return errors.New("Unable to determine VMDK create-type")
		}
		if len(m.cfg.VMDKAllowed) == 0 {
			return errors.New("Image is a VMDK, but no VMDK createType is specified")
		}
		if !slices.Contains(m.cfg.VMDKAllowed, info.Specific.Data.CreateType) {
			return errors.New("Invalid VMDK create-type specified")
		}
	}
	if source == target {
		return nil
	}
	dest := src + "." + target
	if msg, err := exec.CommandContext(run, "qemu-img", "convert", "-f", source, "-O", target, src, dest).CombinedOutput(); err != nil {
		_ = os.Remove(dest)
		return errors.New("qemu-img convert failed: " + string(bytes.TrimSpace(msg)))
	}
	got, err := format.DetectFile(dest, m.cfg.GPTNonfatal)
	if got == "gpt" {
		got = "raw"
	}
	if err != nil || got != target {
		_ = os.Remove(dest)
		return errors.New("Converted image in unexpected format")
	}
	if err = os.Rename(dest, src); err != nil {
		return err
	}
	bare := "bare"
	return m.setImage(ctx, id, func(i *model.Image) {
		i.DiskFormat, i.ContainerFormat = &target, &bare
	})
}
