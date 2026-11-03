package ru.vektor.docs;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.StandardCopyOption;
import java.util.UUID;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Component;

/**
 * Файловое хранилище оригиналов и извлечённого текста.
 *
 * <p>Ключ генерирует сервис: пользовательское имя файла остаётся метаданными и
 * не участвует в пути. Запись идёт во временный файл рядом с целевым и
 * завершается атомарным переименованием, поэтому читатель не увидит половину.
 */
@Component
public class BlobStore {

    private final Path root;

    public BlobStore(@Value("${documents.blob-dir:/data/blobs}") String dir)
        throws IOException {
        this.root = Paths.get(dir).toAbsolutePath().normalize();
        Files.createDirectories(root);
    }

    public String put(byte[] content) throws IOException {
        var key = UUID.randomUUID().toString().replace("-", "");
        var target = resolve(key);
        var staging = Files.createTempFile(root, "upload-", ".tmp");
        try {
            Files.write(staging, content);
            Files.move(
                staging,
                target,
                StandardCopyOption.ATOMIC_MOVE,
                StandardCopyOption.REPLACE_EXISTING
            );
        } finally {
            Files.deleteIfExists(staging);
        }
        return key;
    }

    public byte[] read(String key) throws IOException {
        return Files.readAllBytes(resolve(key));
    }

    public void delete(String key) throws IOException {
        Files.deleteIfExists(resolve(key));
    }

    private Path resolve(String key) {
        var path = root.resolve(key).normalize();
        // Ключ приходит из БД, но проверка дешёвая: путь не должен выйти за root.
        if (!path.startsWith(root)) {
            throw new IllegalArgumentException("blob key escapes storage root");
        }
        return path;
    }
}
