package ru.vektor.docs;

import java.io.ByteArrayInputStream;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.Locale;

import org.apache.pdfbox.pdmodel.PDDocument;
import org.apache.pdfbox.text.PDFTextStripper;
import org.apache.poi.xwpf.extractor.XWPFWordExtractor;
import org.apache.poi.xwpf.usermodel.XWPFDocument;
import org.springframework.stereotype.Component;

@Component
public class Extract {

    public String text(String filename, byte[] content) {
        var name = filename == null ? "" : filename.toLowerCase(Locale.ROOT);
        var text = switch (suffix(name)) {
            case "pdf" -> pdf(content);
            case "docx" -> docx(content);
            case "md", "txt" -> new String(content, StandardCharsets.UTF_8);
            default -> throw Errors.badRequest("поддерживаются .md, .txt, .pdf, .docx");
        };
        var trimmed = text.strip();
        if (trimmed.isEmpty()) {
            throw Errors.badRequest("в файле нет извлекаемого текста");
        }
        return trimmed;
    }

    private static String suffix(String name) {
        var dot = name.lastIndexOf('.');
        return dot < 0 ? "" : name.substring(dot + 1);
    }

    private static String pdf(byte[] content) {
        try (var document = PDDocument.load(content)) {
            return new PDFTextStripper().getText(document);
        } catch (IOException exception) {
            throw Errors.badRequest("не удалось разобрать PDF");
        }
    }

    private static String docx(byte[] content) {
        try (var document = new XWPFDocument(new ByteArrayInputStream(content))) {
            return new XWPFWordExtractor(document).getText();
        } catch (IOException exception) {
            throw Errors.badRequest("не удалось разобрать DOCX");
        }
    }
}
