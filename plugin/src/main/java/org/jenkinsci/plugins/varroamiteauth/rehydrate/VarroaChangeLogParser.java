package org.jenkinsci.plugins.varroamiteauth.rehydrate;

import java.io.File;
import java.io.IOException;
import java.util.ArrayList;
import java.util.List;

import javax.xml.XMLConstants;
import javax.xml.parsers.DocumentBuilder;
import javax.xml.parsers.DocumentBuilderFactory;
import javax.xml.parsers.ParserConfigurationException;

import org.w3c.dom.Document;
import org.w3c.dom.Element;
import org.w3c.dom.NodeList;
import org.xml.sax.SAXException;

import hudson.model.Run;
import hudson.scm.ChangeLogParser;
import hudson.scm.ChangeLogSet;
import hudson.scm.RepositoryBrowser;

/**
 * Reads the {@code varroa-changelog} document the operator writes as a
 * rehydrated build's {@code changelog0.xml} (design.md D1a) into a
 * {@link VarroaChangeLogSet}.
 */
public final class VarroaChangeLogParser extends ChangeLogParser {

    @SuppressWarnings("rawtypes")
    @Override
    public ChangeLogSet<? extends ChangeLogSet.Entry> parse(Run run, RepositoryBrowser<?> browser, File changelogFile)
            throws IOException, SAXException {
        List<VarroaChangeLogSet.Entry> entries = new ArrayList<>();
        if (changelogFile == null || !changelogFile.isFile()) {
            // A missing or absent changelog file loses this run's change
            // set and nothing else (design.md D1a): parse answers an
            // empty set rather than failing the load.
            return new VarroaChangeLogSet(run, browser, entries);
        }

        Document doc;
        try {
            DocumentBuilderFactory factory = DocumentBuilderFactory.newInstance();
            factory.setFeature("http://apache.org/xml/features/disallow-doctype-decl", true);
            factory.setFeature("http://xml.org/sax/features/external-general-entities", false);
            factory.setFeature("http://xml.org/sax/features/external-parameter-entities", false);
            factory.setXIncludeAware(false);
            factory.setExpandEntityReferences(false);
            factory.setAttribute(XMLConstants.ACCESS_EXTERNAL_DTD, "");
            factory.setAttribute(XMLConstants.ACCESS_EXTERNAL_SCHEMA, "");
            DocumentBuilder builder = factory.newDocumentBuilder();
            doc = builder.parse(changelogFile);
        } catch (ParserConfigurationException e) {
            throw new IOException("configure the varroa-changelog XML parser", e);
        }

        NodeList entryNodes = doc.getElementsByTagName("entry");
        for (int i = 0; i < entryNodes.getLength(); i++) {
            Element el = (Element) entryNodes.item(i);
            entries.add(new VarroaChangeLogSet.Entry(
                    text(el, "commitId"),
                    text(el, "author"),
                    text(el, "msg"),
                    parseTimestamp(text(el, "timestamp"))));
        }
        return new VarroaChangeLogSet(run, browser, entries);
    }

    private static String text(Element parent, String tag) {
        NodeList nodes = parent.getElementsByTagName(tag);
        if (nodes.getLength() == 0) {
            return "";
        }
        String content = nodes.item(0).getTextContent();
        return content == null ? "" : content;
    }

    private static long parseTimestamp(String raw) {
        if (raw == null || raw.isEmpty()) {
            return 0L;
        }
        try {
            return Long.parseLong(raw.trim());
        } catch (NumberFormatException e) {
            return 0L;
        }
    }
}
