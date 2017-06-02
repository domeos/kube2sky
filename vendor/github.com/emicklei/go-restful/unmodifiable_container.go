package restful

import (
	"log"
	"net/http"
)

// Unmodifiable returns a Container that has limitiations in its changes:
// - cannot change Router
// - cannot add/remove WebService
// - cannot add/remove filters (container,webservice,route)
func (c *Container) Unmodifiable() unmodifiableContainer {
	for _, eachWebService := range c.webServices {
		eachWebService.dynamicRoutes = false
		cachingRoutes := []Route{}
		for _, eachRoute := range eachWebService.routes {
			allFilters := []FilterFunction{}
			allFilters = append(allFilters, c.containerFilters...)
			allFilters = append(allFilters, eachWebService.filters...)
			allFilters = append(allFilters, eachRoute.Filters...)
			eachRoute.cachedFilters = allFilters
			cachingRoutes = append(cachingRoutes, eachRoute)
		}
		eachWebService.routes = cachingRoutes
	}
	return unmodifiableContainer{Container: c}
}

type unmodifiableContainer struct {
	*Container
}

func (u unmodifiableContainer) Router(aRouter RouteSelector) {
	log.Printf("cannot change Router for this container")
}

func (u *unmodifiableContainer) Add(service *WebService) *unmodifiableContainer {
	log.Printf("cannot Add to this container")
	return u
}

func (u unmodifiableContainer) Remove(ws *WebService) error {
	log.Printf("cannot Remove from this container")
	return nil
}

// Dispatch the incoming Http Request to a matching WebService.
func (u unmodifiableContainer) dispatch(httpWriter http.ResponseWriter, httpRequest *http.Request) {
	writer := httpWriter

	// Detect if compression is needed
	// assume without compression, test for override
	if u.contentEncodingEnabled {
		doCompress, encoding := wantsCompressedResponse(httpRequest)
		if doCompress {
			var err error
			writer, err = NewCompressingResponseWriter(httpWriter, encoding)
			if err != nil {
				log.Print("[restful] unable to install compressor: ", err)
				httpWriter.WriteHeader(http.StatusInternalServerError)
				return
			}
			// CompressingResponseWriter should be closed after all operations are done
			defer func() {
				if compressWriter, ok := writer.(*CompressingResponseWriter); ok {
					compressWriter.Close()
				}
			}()
		}
	}
	// Find best match Route ; err is non nil if no match was found
	_, route, err := u.router.SelectRoute(
		u.webServices,
		httpRequest)
	if err != nil {
		// a non-200 response has already been written
		// run container filters anyway ; they should not touch the response...
		chain := FilterChain{Filters: u.containerFilters, Target: func(req *Request, resp *Response) {
			switch err.(type) {
			case ServiceError:
				ser := err.(ServiceError)
				u.serviceErrorHandleFunc(ser, req, resp)
			}
		}}
		chain.ProcessFilter(NewRequest(httpRequest), NewResponse(writer))
		return
	}
	wrappedRequest, wrappedResponse := route.wrapRequestResponse(writer, httpRequest)
	// compose filter chain
	chain := FilterChain{Filters: route.cachedFilters, Target: func(req *Request, resp *Response) {
		// handle request by route after passing all filters
		route.Function(wrappedRequest, wrappedResponse)
	}}
	chain.ProcessFilter(wrappedRequest, wrappedResponse)
}
